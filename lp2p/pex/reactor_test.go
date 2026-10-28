package pex

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cometbft/cometbft/libs/log"
	"github.com/cometbft/cometbft/lp2p"
	"github.com/cometbft/cometbft/p2p"
	"github.com/cometbft/cometbft/p2p/mock"
	tmp2p "github.com/cometbft/cometbft/proto/tendermint/p2p"
	"github.com/cosmos/gogoproto/proto"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	p2ptest "github.com/libp2p/go-libp2p/core/test"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/stretchr/testify/suite"
)

type reactorSuite struct {
	suite.Suite

	clock   time.Time
	host    host.Host
	book    *AddrBook
	sw      *fakeSwitch
	reactor *Reactor
}

func TestReactorSuite(t *testing.T) {
	suite.Run(t, new(reactorSuite))
}

func (s *reactorSuite) SetupTest() {
	s.clock = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	h, err := libp2p.New(libp2p.NoListenAddrs)
	s.Require().NoError(err)
	s.T().Cleanup(func() { _ = h.Close() })
	s.host = h

	cfg := DefaultConfig()
	cfg.Self = h.ID()
	s.book = NewAddrBook(filepath.Join(s.T().TempDir(), "addrbook.json"), cfg)
	s.book.now = func() time.Time { return s.clock }

	s.sw = &fakeSwitch{peers: p2p.NewPeerSet()}
	s.reactor = NewReactor(s.book, h)
	s.reactor.now = func() time.Time { return s.clock }
	s.reactor.SetLogger(log.NewNopLogger())
	s.reactor.SetSwitch(s.sw)
}

func (s *reactorSuite) SetupSubTest() {
	s.SetupTest()
}

func (s *reactorSuite) newPeer(outbound, supportsPEX bool) *fakePeer {
	id := s.id()
	p := &fakePeer{Peer: mock.NewPeer(nil), id: id, outbound: outbound}
	if supportsPEX {
		s.Require().NoError(s.host.Peerstore().AddProtocols(id, lp2p.ProtocolID(Channel)))
	}

	return p
}

func (s *reactorSuite) id() peer.ID {
	return p2ptest.RandPeerIDFatal(s.T())
}

func netAddr(id peer.ID, ip string, port uint32) tmp2p.NetAddress {
	return tmp2p.NetAddress{ID: id.String(), IP: ip, Port: port}
}

func (s *reactorSuite) TestFilterRateLimitsEveryMessage() {
	request, err := proto.Marshal((&tmp2p.PexRequest{}).Wrap())
	s.Require().NoError(err)

	testCases := []struct {
		name    string
		payload []byte
	}{
		{name: "request", payload: request},
		{name: "addresses", payload: []byte{0x12, 0x00}},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			p := s.newPeer(false, true)

			for range msgBurst {
				s.NoError(s.reactor.FilterMsgBytes(Channel, p, tc.payload))
			}
			s.ErrorIs(s.reactor.FilterMsgBytes(Channel, p, tc.payload), lp2p.ErrDropMessage)

			s.clock = s.clock.Add(msgInterval)
			s.NoError(s.reactor.FilterMsgBytes(Channel, p, tc.payload))
			s.ErrorIs(s.reactor.FilterMsgBytes(Channel, p, tc.payload), lp2p.ErrDropMessage)

			s.clock = s.clock.Add(time.Hour)
			for range msgBurst {
				s.NoError(s.reactor.FilterMsgBytes(Channel, p, tc.payload))
			}
			s.ErrorIs(s.reactor.FilterMsgBytes(Channel, p, tc.payload), lp2p.ErrDropMessage)

			other := s.newPeer(false, true)
			s.NoError(s.reactor.FilterMsgBytes(Channel, other, tc.payload))
		})
	}
}

func (s *reactorSuite) TestRespond() {
	testCases := []struct {
		name     string
		fill     func(requester peer.ID) []tmp2p.NetAddress
		expected int
	}{
		{
			name: "only proven addresses, never the requester",
			fill: func(requester peer.ID) []tmp2p.NetAddress {
				verified, dialed, unproven := s.id(), s.id(), s.id()
				s.Require().NoError(s.book.AddVerified(verified, addr("8.8.8.8", 1), SourceInbound))
				s.Require().NoError(s.book.AddGossiped(dialed, addr("9.9.9.9", 2), s.id()))
				s.book.MarkDialed(dialed)
				s.Require().NoError(s.book.AddGossiped(unproven, addr("7.7.7.7", 3), s.id()))
				s.Require().NoError(s.book.AddVerified(requester, addr("6.6.6.6", 4), SourceInbound))
				return []tmp2p.NetAddress{netAddr(verified, "8.8.8.8", 1), netAddr(dialed, "9.9.9.9", 2)}
			},
			expected: 2,
		},
		{
			name: "at most maxAddrs addresses",
			fill: func(peer.ID) []tmp2p.NetAddress {
				for i := range maxAddrs + 5 {
					s.Require().NoError(s.book.AddVerified(s.id(), addr(fmt.Sprintf("%d.%d.1.1", i/200+1, i%200+1), 1), SourceInbound))
				}
				return nil
			},
			expected: maxAddrs,
		},
		{
			name:     "empty book",
			fill:     func(peer.ID) []tmp2p.NetAddress { return nil },
			expected: 0,
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			requester := s.newPeer(false, true)
			want := tc.fill(requester.id)

			s.reactor.Receive(p2p.Envelope{Src: requester, ChannelID: Channel, Message: &tmp2p.PexRequest{}})

			sent := requester.sentMessages()
			s.Require().Len(sent, 1)
			reply, ok := sent[0].(*tmp2p.PexAddrs)
			s.Require().True(ok)
			s.Len(reply.Addrs, tc.expected)
			if want != nil {
				s.ElementsMatch(want, reply.Addrs)
			}
		})
	}
}

func (s *reactorSuite) TestReceiveAddrs() {
	known, private := s.id(), s.id()

	testCases := []struct {
		name      string
		requested bool
		addrs     func() []tmp2p.NetAddress
		stored    []peer.ID
		punished  bool
	}{
		{
			name:  "unsolicited reply",
			addrs: func() []tmp2p.NetAddress { return []tmp2p.NetAddress{netAddr(known, "8.8.8.8", 1)} },
		},
		{
			name:      "solicited reply",
			requested: true,
			addrs:     func() []tmp2p.NetAddress { return []tmp2p.NetAddress{netAddr(known, "8.8.8.8", 1)} },
			stored:    []peer.ID{known},
		},
		{
			name:      "addresses the book refuses",
			requested: true,
			addrs: func() []tmp2p.NetAddress {
				return []tmp2p.NetAddress{netAddr(s.host.ID(), "8.8.8.8", 1), netAddr(private, "10.0.0.1", 1)}
			},
		},
		{
			name:      "undecodable peer ID",
			requested: true,
			addrs: func() []tmp2p.NetAddress {
				return []tmp2p.NetAddress{netAddr(known, "8.8.8.8", 1), {ID: "nope", IP: "9.9.9.9", Port: 1}}
			},
			punished: true,
		},
		{
			name:      "oversized peer ID",
			requested: true,
			addrs:     func() []tmp2p.NetAddress { return []tmp2p.NetAddress{netAddr(hugePeerID(), "8.8.8.8", 1)} },
			punished:  true,
		},
		{
			name:      "raw peer ID over the book's limit",
			requested: true,
			addrs:     func() []tmp2p.NetAddress { return []tmp2p.NetAddress{netAddr(identityPeerID(63), "8.8.8.8", 1)} },
		},
		{
			name:      "unparsable IP",
			requested: true,
			addrs:     func() []tmp2p.NetAddress { return []tmp2p.NetAddress{{ID: known.String(), IP: "999.1.1.1", Port: 1}} },
			punished:  true,
		},
		{
			name:      "port zero",
			requested: true,
			addrs:     func() []tmp2p.NetAddress { return []tmp2p.NetAddress{netAddr(known, "8.8.8.8", 0)} },
			punished:  true,
		},
		{
			name: "unsolicited malformed reply",
			addrs: func() []tmp2p.NetAddress {
				return []tmp2p.NetAddress{{ID: "nope", IP: "9.9.9.9", Port: 1}}
			},
		},
		{
			name:      "exactly maxAddrs addresses",
			requested: true,
			addrs: func() []tmp2p.NetAddress {
				addrs := make([]tmp2p.NetAddress, maxAddrs)
				for i := range addrs {
					addrs[i] = netAddr(known, fmt.Sprintf("8.8.%d.%d", i/250, i%250+1), 1)
				}
				return addrs
			},
			stored: []peer.ID{known},
		},
		{
			name:      "too many addresses",
			requested: true,
			addrs: func() []tmp2p.NetAddress {
				addrs := make([]tmp2p.NetAddress, maxAddrs+1)
				for i := range addrs {
					addrs[i] = netAddr(known, fmt.Sprintf("8.8.%d.%d", i/250, i%250+1), 1)
				}
				return addrs
			},
			punished: true,
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			src := s.newPeer(true, true)
			if tc.requested {
				s.reactor.request(src)
			}

			s.reactor.Receive(p2p.Envelope{Src: src, ChannelID: Channel, Message: &tmp2p.PexAddrs{Addrs: tc.addrs()}})

			s.Equal(len(tc.stored), s.book.Size())
			for _, id := range tc.stored {
				e, ok := s.book.Get(id)
				s.Require().True(ok)
				s.Equal(src.id, e.Origin)
			}
			s.Equal(tc.punished, s.book.IsBanned(src.id))
			s.Equal(tc.punished, s.sw.stopped(src))
			s.False(s.reactor.hasRequest(src.ID()))
			if tc.punished {
				s.clock = s.clock.Add(24*time.Hour - time.Second)
				s.True(s.book.IsBanned(src.id))
				s.clock = s.clock.Add(2 * time.Second)
				s.False(s.book.IsBanned(src.id))
			}
		})
	}
}

func (s *reactorSuite) TestOneReplyPerRequest() {
	src := s.newPeer(true, true)
	first, second := s.id(), s.id()
	s.reactor.request(src)

	s.reactor.Receive(p2p.Envelope{Src: src, ChannelID: Channel, Message: &tmp2p.PexAddrs{Addrs: []tmp2p.NetAddress{netAddr(first, "8.8.8.8", 1)}}})
	s.reactor.Receive(p2p.Envelope{Src: src, ChannelID: Channel, Message: &tmp2p.PexAddrs{Addrs: []tmp2p.NetAddress{netAddr(second, "9.9.9.9", 1)}}})

	_, ok := s.book.Get(first)
	s.True(ok)
	_, ok = s.book.Get(second)
	s.False(ok)
}

func (s *reactorSuite) TestAddPeerAsks() {
	testCases := []struct {
		name        string
		outbound    bool
		supportsPEX bool
		bookFull    bool
		banned      bool
		asked       bool
	}{
		{name: "outbound PEX peer", outbound: true, supportsPEX: true, asked: true},
		{name: "inbound peer", supportsPEX: true},
		{name: "peer without PEX", outbound: true},
		{name: "book has enough addresses", outbound: true, supportsPEX: true, bookFull: true},
		{name: "banned peer", outbound: true, supportsPEX: true, banned: true},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			if tc.bookFull {
				s.book.cfg.MaxSize = 0
			}
			p := s.newPeer(tc.outbound, tc.supportsPEX)
			if tc.banned {
				s.book.Ban(p.id, s.clock.Add(time.Hour))
			}

			s.reactor.AddPeer(p)

			s.Equal(tc.asked, s.reactor.hasRequest(p.ID()))
			if tc.asked {
				s.Eventually(func() bool { return len(p.sentMessages()) == 1 }, time.Second, 5*time.Millisecond)
				return
			}
			s.Never(func() bool { return len(p.sentMessages()) > 0 }, 50*time.Millisecond, 5*time.Millisecond)
		})
	}
}

func (s *reactorSuite) TestOnePendingRequestPerPeer() {
	p := s.newPeer(true, true)

	s.reactor.request(p)
	s.reactor.request(p)

	s.Eventually(func() bool { return len(p.sentMessages()) == 1 }, time.Second, 5*time.Millisecond)
	s.Never(func() bool { return len(p.sentMessages()) > 1 }, 50*time.Millisecond, 5*time.Millisecond)
}

func (s *reactorSuite) TestFailedRequestIsNotPending() {
	p := s.newPeer(true, true)
	s.reactor.request(p)
	s.clock = s.clock.Add(requestTimeout + time.Second)
	s.reactor.expire([]p2p.Peer{p})
	p.mu.Lock()
	p.failSend = true
	p.mu.Unlock()

	s.reactor.request(p)

	s.Eventually(func() bool { return !s.reactor.hasRequest(p.ID()) }, time.Second, 5*time.Millisecond)
	s.True(s.reactor.timedOut(p.ID()))
}

func (s *reactorSuite) TestAskRandomPeer() {
	inbound := s.newPeer(false, true)
	outbound := s.newPeer(true, true)
	noPEX := s.newPeer(true, false)
	for _, p := range []*fakePeer{inbound, outbound, noPEX} {
		s.Require().NoError(s.sw.peers.Add(p))
	}
	sent := func(p *fakePeer, n int) func() bool {
		return func() bool { return len(p.sentMessages()) == n }
	}

	s.reactor.askRandomPeer()
	s.Eventually(sent(outbound, 1), time.Second, 5*time.Millisecond)
	s.Empty(inbound.sentMessages())
	s.Empty(noPEX.sentMessages())

	s.reactor.askRandomPeer()
	s.Eventually(sent(inbound, 1), time.Second, 5*time.Millisecond)
	s.Len(outbound.sentMessages(), 1)
}

func (s *reactorSuite) TestAskPrefersPeersThatDidNotTimeOut() {
	for range 5 {
		s.SetupTest()
		silent, other := s.newPeer(true, true), s.newPeer(true, true)
		s.Require().NoError(s.sw.peers.Add(silent))
		s.reactor.askRandomPeer()
		s.Eventually(func() bool { return len(silent.sentMessages()) == 1 }, time.Second, 5*time.Millisecond)

		s.Require().NoError(s.sw.peers.Add(other))
		s.clock = s.clock.Add(requestTimeout + time.Second)
		s.reactor.askRandomPeer()

		s.Eventually(func() bool { return len(other.sentMessages()) == 1 }, time.Second, 5*time.Millisecond)
		s.Len(silent.sentMessages(), 1)
		s.False(s.reactor.hasRequest(silent.ID()))
	}
}

func (s *reactorSuite) TestReplyClearsTimeout() {
	p := s.newPeer(true, true)
	s.reactor.request(p)
	s.clock = s.clock.Add(requestTimeout + time.Second)
	s.reactor.expire([]p2p.Peer{p})
	s.Require().True(s.reactor.timedOut(p.ID()))

	s.reactor.request(p)
	s.reactor.Receive(p2p.Envelope{Src: p, ChannelID: Channel, Message: &tmp2p.PexAddrs{}})

	s.False(s.reactor.timedOut(p.ID()))
}

func (s *reactorSuite) TestAskRandomPeerSkipsWhenBookHasEnough() {
	s.book.cfg.MaxSize = 0
	p := s.newPeer(true, true)
	s.Require().NoError(s.sw.peers.Add(p))

	s.reactor.askRandomPeer()

	s.False(s.reactor.hasRequest(p.ID()))
}

func (s *reactorSuite) TestAskRandomPeerForgetsDepartedPeers() {
	departed, pending := s.newPeer(true, true), s.newPeer(true, true)
	s.Require().NoError(s.reactor.FilterMsgBytes(Channel, departed, nil))
	s.reactor.request(pending)

	s.reactor.askRandomPeer()

	s.reactor.mu.Lock()
	defer s.reactor.mu.Unlock()
	s.NotContains(s.reactor.peers, departed.ID())
	s.Contains(s.reactor.peers, pending.ID())
}

func (s *reactorSuite) TestTickAsksAndSaves() {
	s.reactor.askEvery = 10 * time.Millisecond
	s.reactor.saveEvery = 10 * time.Millisecond
	p := s.newPeer(true, true)
	s.Require().NoError(s.sw.peers.Add(p))

	s.Require().NoError(s.reactor.Start())
	s.T().Cleanup(func() { _ = s.reactor.Stop() })

	s.Eventually(func() bool { return len(p.sentMessages()) == 1 }, time.Second, 5*time.Millisecond)
	s.Eventually(func() bool { _, err := os.Stat(s.book.path); return err == nil }, time.Second, 5*time.Millisecond)
}

func (s *reactorSuite) TestStopWaitsForInFlightRequest() {
	p := s.newPeer(true, true)
	p.block = make(chan struct{})
	s.Require().NoError(s.reactor.Start())
	s.reactor.request(p)

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = s.reactor.Stop()
	}()
	isStopped := func() bool {
		select {
		case <-stopped:
			return true
		default:
			return false
		}
	}

	s.Never(isStopped, 100*time.Millisecond, 10*time.Millisecond)
	close(p.block)
	s.Eventually(isStopped, time.Second, 10*time.Millisecond)
}

func (s *reactorSuite) TestNoRequestAfterStop() {
	s.Require().NoError(s.reactor.Start())
	s.Require().NoError(s.reactor.Stop())
	p := s.newPeer(true, true)

	s.reactor.request(p)

	s.False(s.reactor.hasRequest(p.ID()))
	s.Never(func() bool { return len(p.sentMessages()) > 0 }, 50*time.Millisecond, 5*time.Millisecond)
}

func (s *reactorSuite) TestRemovePeerForgetsState() {
	p := s.newPeer(true, true)
	s.reactor.request(p)
	for range msgBurst {
		s.Require().NoError(s.reactor.FilterMsgBytes(Channel, p, nil))
	}

	s.reactor.RemovePeer(p, nil)

	s.False(s.reactor.hasRequest(p.ID()))
	s.NoError(s.reactor.FilterMsgBytes(Channel, p, nil))
}

func (s *reactorSuite) TestMarkPeerAsGood() {
	p := s.newPeer(true, true)
	s.Require().NoError(s.book.AddVerified(p.id, addr("8.8.8.8", 1), SourceInbound))

	s.reactor.MarkPeerAsGood(p)

	e, _ := s.book.Get(p.id)
	s.Equal(s.clock, e.LastGood)
}

func (s *reactorSuite) TestVerifyInboundAddrs() {
	listen := []ma.Multiaddr{addr("8.8.8.8", 26656)}

	pexProtocol := []protocol.ID{lp2p.ProtocolID(Channel)}

	testCases := []struct {
		name      string
		dir       network.Direction
		remote    ma.Multiaddr
		protocols []protocol.ID
		verified  bool
	}{
		{name: "inbound PEX peer from its listen address", dir: network.DirInbound, remote: addr("8.8.8.8", 26656), protocols: pexProtocol, verified: true},
		{name: "inbound peer without PEX", dir: network.DirInbound, remote: addr("8.8.8.8", 26656)},
		{name: "inbound from another port", dir: network.DirInbound, remote: addr("8.8.8.8", 40000), protocols: pexProtocol},
		{name: "outbound", dir: network.DirOutbound, remote: addr("8.8.8.8", 26656), protocols: pexProtocol},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			s.Require().NoError(s.reactor.Start())
			s.T().Cleanup(func() { _ = s.reactor.Stop() })

			emitter, err := s.host.EventBus().Emitter(new(event.EvtPeerIdentificationCompleted))
			s.Require().NoError(err)
			defer emitter.Close()

			id, sentinel := s.id(), s.id()
			s.Require().NoError(emitter.Emit(event.EvtPeerIdentificationCompleted{
				Peer:        id,
				Conn:        &fakeConn{dir: tc.dir, remote: tc.remote},
				ListenAddrs: listen,
				Protocols:   tc.protocols,
			}))
			s.Require().NoError(emitter.Emit(event.EvtPeerIdentificationCompleted{
				Peer:        sentinel,
				Conn:        &fakeConn{dir: network.DirInbound, remote: addr("9.9.9.9", 26656)},
				ListenAddrs: []ma.Multiaddr{addr("9.9.9.9", 26656)},
				Protocols:   pexProtocol,
			}))

			s.Eventually(func() bool { _, ok := s.book.Get(sentinel); return ok }, time.Second, 10*time.Millisecond)
			_, ok := s.book.Get(id)
			s.Equal(tc.verified, ok)
		})
	}
}

func (s *reactorSuite) TestStartLoadsAndStopSaves() {
	id := s.id()
	s.Require().NoError(s.book.AddVerified(id, addr("8.8.8.8", 1), SourceInbound))
	s.Require().NoError(s.reactor.Start())
	s.Require().NoError(s.reactor.Stop())

	loaded := NewAddrBook(s.book.path, s.book.cfg)
	restarted := NewReactor(loaded, s.host)
	restarted.SetLogger(log.NewNopLogger())
	s.Require().NoError(restarted.Start())
	s.T().Cleanup(func() { _ = restarted.Stop() })

	_, ok := loaded.Get(id)
	s.True(ok)
}

func (s *reactorSuite) TestConcurrentUse() {
	s.reactor.now = time.Now
	peers := make([]*fakePeer, 8)
	for i := range peers {
		peers[i] = s.newPeer(i%2 == 0, true)
		s.Require().NoError(s.sw.peers.Add(peers[i]))
	}

	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Go(func() {
			for range 20 {
				_ = s.reactor.FilterMsgBytes(Channel, p, nil)
				s.reactor.request(p)
				s.reactor.Receive(p2p.Envelope{Src: p, ChannelID: Channel, Message: &tmp2p.PexAddrs{}})
				s.reactor.Receive(p2p.Envelope{Src: p, ChannelID: Channel, Message: &tmp2p.PexRequest{}})
				s.reactor.askRandomPeer()
				s.reactor.RemovePeer(p, nil)
			}
		})
	}
	wg.Wait()
}

func (s *reactorSuite) TestStartWithUnreadableBook() {
	s.Require().NoError(os.WriteFile(s.book.path, []byte("{"), 0o600))

	s.Require().NoError(s.reactor.Start())
	s.T().Cleanup(func() { _ = s.reactor.Stop() })

	s.Zero(s.book.Size())
}

func (s *reactorSuite) TestParseNetAddress() {
	id := s.id()

	testCases := []struct {
		name     string
		na       tmp2p.NetAddress
		expected ma.Multiaddr
	}{
		{name: "IPv4", na: netAddr(id, "8.8.8.8", 1), expected: addr("8.8.8.8", 1)},
		{name: "IPv4-mapped IPv6", na: netAddr(id, "::ffff:8.8.8.8", 1), expected: addr("8.8.8.8", 1)},
		{name: "IPv6", na: netAddr(id, "2001:4860::1", 1), expected: addr("2001:4860::1", 1)},
		{name: "highest port", na: netAddr(id, "8.8.8.8", 65535), expected: addr("8.8.8.8", 65535)},
		{name: "port above range", na: netAddr(id, "8.8.8.8", 65536)},
		{name: "undecodable peer ID", na: tmp2p.NetAddress{ID: "z0OIl", IP: "8.8.8.8", Port: 1}},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			gotID, got, err := parseNetAddress(tc.na)
			if tc.expected == nil {
				s.Require().Error(err)
				s.NotContains(err.Error(), tc.na.ID)
				return
			}
			s.Require().NoError(err)
			s.Equal(id, gotID)
			s.True(tc.expected.Equal(got))
		})
	}
}

type fakeSwitch struct {
	p2p.Switcher

	peers *p2p.PeerSet

	mu         sync.Mutex
	stoppedIDs []p2p.ID
}

func (sw *fakeSwitch) Peers() p2p.IPeerSet { return sw.peers }

func (sw *fakeSwitch) StopPeerForError(p p2p.Peer, _ any) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	sw.stoppedIDs = append(sw.stoppedIDs, p.ID())
}

func (sw *fakeSwitch) stopped(p p2p.Peer) bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	return slices.Contains(sw.stoppedIDs, p.ID())
}

type fakePeer struct {
	*mock.Peer

	id       peer.ID
	outbound bool
	failSend bool
	block    chan struct{}

	mu   sync.Mutex
	sent []p2p.Envelope
}

func (p *fakePeer) ID() p2p.ID               { return p2p.ID(p.id.String()) }
func (p *fakePeer) IsOutbound() bool         { return p.outbound }
func (p *fakePeer) RemoteIP() net.IP         { return net.IPv4(8, 8, 4, 4) }
func (p *fakePeer) Send(e p2p.Envelope) bool { return p.TrySend(e) }

func (p *fakePeer) TrySend(e p2p.Envelope) bool {
	if p.block != nil {
		<-p.block
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.failSend {
		return false
	}
	p.sent = append(p.sent, e)

	return true
}

func (p *fakePeer) sentMessages() []any {
	p.mu.Lock()
	defer p.mu.Unlock()

	msgs := make([]any, len(p.sent))
	for i, e := range p.sent {
		msgs[i] = e.Message
	}

	return msgs
}

type fakeConn struct {
	network.Conn

	dir    network.Direction
	remote ma.Multiaddr
}

func (c *fakeConn) Stat() network.ConnStats {
	return network.ConnStats{Stats: network.Stats{Direction: c.dir}}
}

func (c *fakeConn) RemoteMultiaddr() ma.Multiaddr { return c.remote }
