package lp2p

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/libs/log"
	"github.com/cometbft/cometbft/p2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

func TestPeer(t *testing.T) {
	t.Run("PeerInfo", func(t *testing.T) {
		// ARRANGE
		ctx := context.Background()

		hosts := makeTestHosts(t, 3)
		hostA := hosts[0]
		hostB := hosts[1]
		hostC := hosts[2]

		// hostA dials hostB (outbound from A's perspective)
		err := hostA.Connect(ctx, hostB.AddrInfo())
		require.NoError(t, err)

		// hostC dials hostA (inbound from A's perspective)
		err = hostC.Connect(ctx, hostA.AddrInfo())
		require.NoError(t, err)

		peerB, err := NewPeer(hostA, hostB.AddrInfo(), p2p.NopMetrics(), false, false, false)
		require.NoError(t, err)

		peerC, err := NewPeer(hostA, hostC.AddrInfo(), p2p.NopMetrics(), false, false, false)
		require.NoError(t, err)

		// ACT & ASSERT — outbound peer (we dialed hostB)
		// NodeInfo must type-assert to DefaultNodeInfo (this is what /net_info does)
		nodeInfo, ok := peerB.NodeInfo().(p2p.DefaultNodeInfo)
		require.True(t, ok, "NodeInfo must type-assert to p2p.DefaultNodeInfo")
		assert.Equal(t, peerB.ID(), nodeInfo.DefaultNodeID)
		assert.NotEmpty(t, nodeInfo.ListenAddr)

		assert.True(t, peerB.RemoteIP().Equal(net.IPv4(127, 0, 0, 1)))
		assert.True(t, peerB.IsOutbound())

		remoteAddr, ok := peerB.RemoteAddr().(*net.TCPAddr)
		require.True(t, ok)
		assert.NotZero(t, remoteAddr.Port)

		// ACT & ASSERT — inbound peer (hostC dialed us)
		_, ok = peerC.NodeInfo().(p2p.DefaultNodeInfo)
		require.True(t, ok)
		assert.True(t, peerC.IsOutbound(), "all lp2p peers are bi-directional")
	})

	t.Run("NetInfoIteration", func(t *testing.T) {
		// Simulates the /net_info ForEach loop from rpc/core/net.go
		// to verify the libp2p Peer works end-to-end with the RPC handler.
		ctx := context.Background()

		hosts := makeTestHosts(t, 4)
		hostA := hosts[0]

		ps := NewPeerSet(hostA, p2p.NopMetrics(), log.NewNopLogger())

		for i := 1; i < len(hosts); i++ {
			err := hostA.Connect(ctx, hosts[i].AddrInfo())
			require.NoError(t, err)

			_, err = ps.Add(hosts[i].AddrInfo(), PeerAddOptions{})
			require.NoError(t, err)
		}

		// ACT — exact pattern from rpc/core/net.go NetInfo()
		count := 0

		ps.ForEach(func(peer p2p.Peer) {
			// These are the fields /net_info accesses
			_, ok := peer.NodeInfo().(p2p.DefaultNodeInfo)
			require.True(t, ok, "NodeInfo must type-assert to p2p.DefaultNodeInfo")
			_ = peer.IsOutbound()
			_ = peer.RemoteIP().String()
			_ = peer.Status()

			count++
		})

		assert.Equal(t, 3, count)
	})

	t.Run("GetSet", func(t *testing.T) {
		// ARRANGE
		hosts := makeTestHosts(t, 2)
		hostA := hosts[0]
		hostB := hosts[1]

		peerB, err := NewPeer(hostA, hostB.AddrInfo(), p2p.NopMetrics(), false, false, false)
		require.NoError(t, err)

		const key = "test-key"
		expected := map[string]int{"value": 42}

		// ACT
		got1 := peerB.Get(key)
		peerB.Set(key, expected)
		actual := peerB.Get(key)

		// ASSERT
		require.Nil(t, got1)
		require.NotNil(t, actual)
		require.Equal(t, expected, actual)
	})

	t.Run("Send", func(t *testing.T) {
		// ARRANGE
		var (
			ctx   = context.Background()
			hosts = makeTestHosts(t, 2)
			hostA = hosts[0]
			hostB = hosts[1]
		)

		err := hostA.Connect(ctx, hostB.AddrInfo())
		require.NoError(t, err)

		// given peerB in hostA
		peerB, err := NewPeer(hostA, hostB.AddrInfo(), p2p.NopMetrics(), false, false, false)
		require.NoError(t, err)

		const channelID = byte(0xaa)

		payloadCh := make(chan []byte, 10)
		defer close(payloadCh)

		hostB.SetStreamHandler(ProtocolID(channelID), func(stream network.Stream) {
			payload, err := StreamReadClose(stream)
			require.NoError(t, err)

			payloadCh <- payload
		})

		// ACT
		// send two messages
		send1 := peerB.Send(p2p.Envelope{
			ChannelID: channelID,
			Message:   types.ToRequestEcho("hello-send"),
		})
		send2 := peerB.TrySend(p2p.Envelope{
			ChannelID: channelID,
			Message:   types.ToRequestEcho("hello-try-send"),
		})

		// ASSERT
		require.True(t, send1)
		require.True(t, send2)

		// check msg1
		msg1 := &types.Request{}
		require.NoError(t, msg1.Unmarshal(<-payloadCh))

		// check msg2
		msg2 := &types.Request{}
		require.NoError(t, msg2.Unmarshal(<-payloadCh))

		require.ElementsMatch(
			t,
			[]string{"hello-send", "hello-try-send"},
			[]string{msg1.GetEcho().GetMessage(), msg2.GetEcho().GetMessage()},
		)

		t.Run("Failure", func(t *testing.T) {
			// ACT
			send := peerB.Send(p2p.Envelope{
				ChannelID: channelID,
				Message:   nil, // invalid proto message
			})

			// ASSERT
			require.False(t, send)
		})
	})
}

type peerRedialSuite struct {
	suite.Suite
}

func TestPeerRedialSuite(t *testing.T) {
	suite.Run(t, new(peerRedialSuite))
}

func (s *peerRedialSuite) newPeerPair(bootstrap bool) (*Host, *Host, chan peer.ID) {
	hosts := makeTestHosts(s.T(), 2)
	hostA, hostB := hosts[0], hosts[1]

	if bootstrap {
		hostA.bootstrapPeers[hostB.ID()] = BootstrapPeer{AddrInfo: hostB.AddrInfo()}
	}

	failures := make(chan peer.ID, 1)
	hostA.AddPeerFailureHandler(func(id peer.ID, _ error) {
		select {
		case failures <- id:
		default:
		}
	})

	return hostA, hostB, failures
}

func (s *peerRedialSuite) assertFailureReported(failures chan peer.ID) {
	select {
	case <-failures:
	case <-time.After(5 * time.Second):
		s.Fail("peer failure was not reported")
	}
}

func (s *peerRedialSuite) assertNoFailure(failures chan peer.ID) {
	select {
	case <-failures:
		s.Fail("unexpected peer failure")
	case <-time.After(300 * time.Millisecond):
	}
}

func (s *peerRedialSuite) TestSendFailure() {
	testCases := []struct {
		name            string
		bootstrap       bool
		disconnect      bool
		noHandler       bool
		expectSent      bool
		expectConnected bool
		expectFailure   bool
	}{
		{name: "disconnected bootstrap peer is redialed", bootstrap: true, disconnect: true, expectSent: true, expectConnected: true},
		{name: "disconnected other peer is reported, not redialed", disconnect: true, expectFailure: true},
		{name: "connected other peer with a failed send is kept", noHandler: true, expectConnected: true},
		{name: "connected bootstrap peer with a failed send is kept", bootstrap: true, noHandler: true, expectConnected: true},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			const channelID = byte(0xad)

			hostA, hostB, failures := s.newPeerPair(tc.bootstrap)

			if !tc.noHandler {
				hostB.SetStreamHandler(ProtocolID(channelID), func(stream network.Stream) {
					_, _ = StreamReadClose(stream)
				})
			}

			s.Require().NoError(hostA.Connect(context.Background(), hostB.AddrInfo()))

			if tc.disconnect {
				s.Require().NoError(hostA.Network().ClosePeer(hostB.ID()))
			}

			peerB, err := NewPeer(hostA, hostB.AddrInfo(), p2p.NopMetrics(), false, false, false)
			s.Require().NoError(err)

			sent := peerB.Send(p2p.Envelope{ChannelID: channelID, Message: types.ToRequestEcho("hi")})

			s.Equal(tc.expectSent, sent)
			s.Equal(tc.expectConnected, hostA.Network().Connectedness(hostB.ID()) == network.Connected)

			if tc.expectFailure {
				s.assertFailureReported(failures)
				return
			}

			s.assertNoFailure(failures)
		})
	}
}

func (s *peerRedialSuite) TestHandleSendErr() {
	testCases := []struct {
		name       string
		bootstrap  bool
		err        error
		expectEmit bool
	}{
		{name: "disconnected other peer with a non-dial error", err: errors.New("connection failed"), expectEmit: true},
		{name: "disconnected other peer whose dials failed", err: swarm.ErrAllDialsFailed, expectEmit: true},
		{name: "disconnected bootstrap peer with a non-dial error", bootstrap: true, err: errors.New("connection failed")},
		{name: "disconnected bootstrap peer whose dials failed", bootstrap: true, err: swarm.ErrAllDialsFailed, expectEmit: true},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			hostA, hostB, failures := s.newPeerPair(tc.bootstrap)

			peerB, err := NewPeer(hostA, hostB.AddrInfo(), p2p.NopMetrics(), false, false, false)
			s.Require().NoError(err)

			peerB.handleSendErr(tc.err)

			if tc.expectEmit {
				s.assertFailureReported(failures)
				return
			}

			s.assertNoFailure(failures)
		})
	}
}

func (s *peerRedialSuite) TestSwitchRemovesUnreachablePeer() {
	hostA, hostB, _ := s.newPeerPair(false)

	sw, err := NewSwitch(nil, hostA, nil, p2p.NopMetrics(), hostA.Logger())
	s.Require().NoError(err)
	s.Require().NoError(sw.Start())
	s.T().Cleanup(func() { _ = sw.Stop() })

	s.Require().NoError(hostA.Connect(context.Background(), hostB.AddrInfo()))

	added, err := sw.peerSet.Add(hostB.AddrInfo(), PeerAddOptions{})
	s.Require().NoError(err)
	s.Require().NoError(hostA.Network().ClosePeer(hostB.ID()))

	s.False(added.Send(p2p.Envelope{ChannelID: 0xad, Message: types.ToRequestEcho("hi")}))

	s.Eventually(func() bool { return sw.Peers().Get(added.ID()) == nil }, 5*time.Second, 20*time.Millisecond)
	s.Never(func() bool {
		return hostA.Network().Connectedness(hostB.ID()) == network.Connected
	}, 500*time.Millisecond, 50*time.Millisecond)
}
