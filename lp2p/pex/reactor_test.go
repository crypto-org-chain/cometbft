package pex

import (
	"context"
	"errors"
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
	"github.com/libp2p/go-libp2p/core/sec"
	p2ptest "github.com/libp2p/go-libp2p/core/test"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/stretchr/testify/suite"
)

type reactorSuite struct {
	suite.Suite

	clock   time.Time
	host    host.Host
	net     *fakeNetwork
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
	s.net = &fakeNetwork{Network: h.Network(), conns: map[peer.ID][]network.Conn{}}
	s.reactor = NewReactor(s.book, &fakeHost{Host: h, net: s.net}, 10)
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
		private     bool
		asked       bool
	}{
		{name: "outbound PEX peer", outbound: true, supportsPEX: true, asked: true},
		{name: "inbound peer", supportsPEX: true},
		{name: "peer without PEX", outbound: true},
		{name: "book has enough addresses", outbound: true, supportsPEX: true, bookFull: true},
		{name: "banned peer", outbound: true, supportsPEX: true, banned: true},
		{name: "private peer", outbound: true, supportsPEX: true, private: true},
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
			if tc.private {
				s.book.cfg.Private[p.id] = struct{}{}
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
	restarted := NewReactor(loaded, s.host, 10)
	restarted.SetLogger(log.NewNopLogger())
	restarted.SetSwitch(s.sw)
	s.Require().NoError(restarted.Start())
	s.T().Cleanup(func() { _ = restarted.Stop() })

	_, ok := loaded.Get(id)
	s.True(ok)
}

func (s *reactorSuite) TestStartNeedsADialingSwitch() {
	r := NewReactor(s.book, s.host, 10)
	r.SetLogger(log.NewNopLogger())
	r.SetSwitch(struct{ p2p.Switcher }{})

	s.Require().ErrorContains(r.Start(), "switch cannot dial peers")
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

func (s *reactorSuite) TestDialPeersFillsOutboundSlots() {
	testCases := []struct {
		name     string
		outbound int
		ips      []string
		dialed   int
	}{
		{name: "dials up to the free slots", outbound: 8, ips: []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}, dialed: 2},
		{name: "no free slots", outbound: 10, ips: []string{"1.1.1.1"}},
		{name: "one dial per IP group", ips: []string{"1.1.1.1", "1.1.1.1"}, dialed: 1},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			s.sw.outbound = tc.outbound
			for i, ip := range tc.ips {
				s.Require().NoError(s.book.AddVerified(s.id(), addr(ip, i+1), SourceInbound))
			}

			s.reactor.dialPeers()

			s.Eventually(func() bool { return len(s.sw.dialedIDs()) == tc.dialed }, time.Second, 5*time.Millisecond)
			s.Never(func() bool { return len(s.sw.dialedIDs()) > tc.dialed }, 50*time.Millisecond, 5*time.Millisecond)
			s.waitForDials()
		})
	}
}

func (s *reactorSuite) TestDialPeersWaitsOutTheGroupInterval() {
	s.book.cfg.AllowDuplicateIP = true
	for port := 1; port <= 2; port++ {
		s.Require().NoError(s.book.AddVerified(s.id(), addr("1.1.1.1", port), SourceInbound))
	}
	s.sw.dialFn = func(context.Context, peer.AddrInfo) error { return errors.New("unreachable") }

	s.reactor.dialPeers()
	s.Eventually(func() bool { return len(s.sw.dialedIDs()) == 1 }, time.Second, 5*time.Millisecond)

	s.waitForDials()
	s.clock = s.clock.Add(groupDialInterval)
	s.reactor.dialPeers()
	s.Eventually(func() bool { return len(s.sw.dialedIDs()) == 2 }, time.Second, 5*time.Millisecond)
	s.waitForDials()
}

// waitForDials keeps background dials from reading the clock after the test moves on.
func (s *reactorSuite) waitForDials() {
	s.Eventually(func() bool {
		s.reactor.mu.Lock()
		defer s.reactor.mu.Unlock()
		return s.reactor.dialing == 0
	}, time.Second, 5*time.Millisecond)
}

func (s *reactorSuite) TestDialsInFlightCountTowardOutbound() {
	s.sw.outbound = 9
	release := make(chan struct{})
	s.sw.dialFn = func(context.Context, peer.AddrInfo) error {
		<-release
		return nil
	}
	for i, ip := range []string{"1.1.1.1", "2.2.2.2"} {
		s.Require().NoError(s.book.AddVerified(s.id(), addr(ip, i+1), SourceInbound))
	}

	s.reactor.dialPeers()
	s.Eventually(func() bool { return len(s.sw.dialedIDs()) == 1 }, time.Second, 5*time.Millisecond)
	for range 10 {
		s.reactor.dialPeers()
	}

	s.Never(func() bool { return len(s.sw.dialedIDs()) > 1 }, 50*time.Millisecond, 5*time.Millisecond)
	close(release)
	s.waitForDials()
}

func (s *reactorSuite) TestNoDialAfterStop() {
	s.Require().NoError(s.book.AddVerified(s.id(), addr("1.1.1.1", 1), SourceInbound))
	s.Require().NoError(s.reactor.Start())
	s.Require().NoError(s.reactor.Stop())

	s.reactor.dialPeers()

	s.Never(func() bool { return len(s.sw.dialedIDs()) > 0 }, 50*time.Millisecond, 5*time.Millisecond)
}

func (s *reactorSuite) TestRecordDial() {
	dialed := addr("8.8.8.8", 1)

	testCases := []struct {
		name        string
		err         error
		connectedAt ma.Multiaddr
		moved       bool
		kept        bool
		dialed      bool
		cooldown    bool
		attempts    int
	}{
		{name: "success at the dialed address", connectedAt: dialed, kept: true, dialed: true},
		{name: "success at another address", connectedAt: addr("9.9.9.9", 1), kept: true},
		{name: "peer ID mismatch", err: fmt.Errorf("dial: %w", sec.ErrPeerIDMismatch{})},
		{
			name: "peer ID mismatch at the dialed address",
			err:  &swarm.DialError{DialErrors: []swarm.TransportError{{Address: dialed, Cause: sec.ErrPeerIDMismatch{}}}},
		},
		{
			name:     "peer ID mismatch at another known address",
			err:      &swarm.DialError{DialErrors: []swarm.TransportError{{Address: addr("9.9.9.9", 1), Cause: sec.ErrPeerIDMismatch{}}}},
			kept:     true,
			attempts: 1,
		},
		{name: "peer ID mismatch after the address changed", err: sec.ErrPeerIDMismatch{}, moved: true, kept: true},
		{name: "busy peer", err: lp2p.ErrDialBusy, kept: true, cooldown: true},
		{name: "already connected peer", err: lp2p.ErrPeerConnected, kept: true, cooldown: true},
		{name: "gater", err: swarm.ErrGaterDisallowedConnection, kept: true, cooldown: true},
		{name: "dial backoff", err: swarm.ErrDialBackoff, kept: true, cooldown: true},
		{
			name: "dial backoff only at another known address",
			err: &swarm.DialError{DialErrors: []swarm.TransportError{
				{Address: addr("9.9.9.9", 1), Cause: swarm.ErrDialBackoff},
				{Address: dialed, Cause: context.DeadlineExceeded},
			}},
			kept:     true,
			attempts: 1,
		},
		{
			name:     "gater refusal of the peer",
			err:      &swarm.DialError{Cause: swarm.ErrGaterDisallowedConnection},
			kept:     true,
			cooldown: true,
		},
		{name: "resource limit", err: network.ErrResourceLimitExceeded, kept: true, cooldown: true},
		{name: "inactive switch", err: lp2p.ErrSwitchInactive, kept: true},
		{name: "our own shutdown", err: context.Canceled, kept: true},
		{name: "timeout", err: context.DeadlineExceeded, kept: true, attempts: 1},
		{name: "remote failure", err: errors.New("connection refused"), kept: true, attempts: 1},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			id := s.id()
			s.Require().NoError(s.book.AddGossiped(id, dialed, s.id()))
			e, _ := s.book.Get(id)
			if tc.connectedAt != nil {
				s.net.conns[id] = []network.Conn{&fakeConn{remote: tc.connectedAt}}
			}
			if tc.moved {
				s.Require().NoError(s.book.AddVerified(id, addr("9.9.9.9", 1), SourceInbound))
			}

			s.reactor.recordDial(e, tc.err)

			got, ok := s.book.Get(id)
			s.Require().Equal(tc.kept, ok)
			if !ok {
				return
			}
			s.Equal(tc.attempts, got.Attempts)
			s.Equal(tc.dialed, !got.LastDialedOK.IsZero())
			s.Equal(tc.cooldown, s.book.InCooldown(id))
			if tc.dialed {
				s.Equal(SourceDialed, got.Source)
			}
		})
	}
}

func (s *reactorSuite) TestDialPeersMixesProvenAndUnproven() {
	s.sw.outbound = 7
	proven := make([]peer.ID, 5)
	for i := range proven {
		proven[i] = s.id()
		s.Require().NoError(s.book.AddVerified(proven[i], addr(fmt.Sprintf("%d.1.1.1", i+1), 1), SourceInbound))
		s.book.MarkDialed(proven[i])
	}
	for i := range 20 {
		s.Require().NoError(s.book.AddVerified(s.id(), addr(fmt.Sprintf("%d.2.2.2", i+21), 1), SourceInbound))
	}
	s.clock = s.clock.Add(2 * time.Minute)

	s.reactor.dialPeers()

	s.Eventually(func() bool { return len(s.sw.dialedIDs()) == 3 }, time.Second, 5*time.Millisecond)
	dialedProven := 0
	for _, id := range s.sw.dialedIDs() {
		if slices.Contains(proven, id) {
			dialedProven++
		}
	}
	s.Equal(2, dialedProven)
	s.waitForDials()
}

func (s *reactorSuite) TestReserveDialForgetsExpiredGroups() {
	s.reactor.groupDials["old"] = s.clock.Add(-groupDialInterval)

	s.True(s.reactor.reserveDial(Entry{ip: net.ParseIP("1.1.1.1")}))

	s.NotContains(s.reactor.groupDials, "old")
	s.reactor.workers.Done()
}

func (s *reactorSuite) TestDialPeersSkippedGroupsDoNotCostASlot() {
	for i := range 5 {
		s.Run(fmt.Sprintf("shuffle %d", i), func() {
			s.sw.outbound = 9
			for port := 1; port <= 2; port++ {
				s.Require().NoError(s.book.AddVerified(s.id(), addr("1.1.1.1", port), SourceInbound))
			}
			free := s.id()
			s.Require().NoError(s.book.AddVerified(free, addr("2.2.2.2", 1), SourceInbound))
			s.reactor.groupDials[ipGroup(net.ParseIP("1.1.1.1"))] = s.clock

			s.reactor.dialPeers()

			s.Eventually(func() bool { return slices.Equal(s.sw.dialedIDs(), []peer.ID{free}) }, time.Second, 5*time.Millisecond)
			s.waitForDials()
		})
	}
}

func (s *reactorSuite) TestStopCancelsDials() {
	s.reactor.dialEvery = 10 * time.Millisecond
	id := s.id()
	s.Require().NoError(s.book.AddVerified(id, addr("8.8.8.8", 1), SourceInbound))
	started := make(chan struct{}, 1)
	s.sw.dialFn = func(ctx context.Context, _ peer.AddrInfo) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	s.Require().NoError(s.reactor.Start())
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		s.FailNow("dial did not start")
	}

	s.Require().NoError(s.reactor.Stop())

	e, _ := s.book.Get(id)
	s.Zero(e.Attempts)
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

	peers    *p2p.PeerSet
	outbound int
	dialFn   func(ctx context.Context, addrInfo peer.AddrInfo) error

	mu         sync.Mutex
	stoppedIDs []p2p.ID
	dialed     []peer.ID
}

func (sw *fakeSwitch) NumPeers() (int, int, int) { return sw.outbound, 0, 0 }

func (sw *fakeSwitch) DialPeer(ctx context.Context, addrInfo peer.AddrInfo) error {
	sw.mu.Lock()
	sw.dialed = append(sw.dialed, addrInfo.ID)
	sw.mu.Unlock()

	if sw.dialFn == nil {
		return nil
	}

	return sw.dialFn(ctx, addrInfo)
}

func (sw *fakeSwitch) dialedIDs() []peer.ID {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	return slices.Clone(sw.dialed)
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

type fakeHost struct {
	host.Host
	net network.Network
}

func (h *fakeHost) Network() network.Network { return h.net }

type fakeNetwork struct {
	network.Network
	conns map[peer.ID][]network.Conn
}

func (n *fakeNetwork) ConnsToPeer(id peer.ID) []network.Conn { return n.conns[id] }

type fakeConn struct {
	network.Conn

	dir    network.Direction
	remote ma.Multiaddr
}

func (c *fakeConn) Stat() network.ConnStats {
	return network.ConnStats{Stats: network.Stats{Direction: c.dir}}
}

func (c *fakeConn) RemoteMultiaddr() ma.Multiaddr { return c.remote }
