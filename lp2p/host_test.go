package lp2p

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/crypto/ed25519"
	"github.com/cometbft/cometbft/libs/log"
	"github.com/cometbft/cometbft/p2p"
	"github.com/cometbft/cometbft/test/utils"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	"github.com/libp2p/go-libp2p/p2p/protocol/identify"
	"github.com/libp2p/go-libp2p/p2p/protocol/ping"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/stretchr/testify/require"
)

type connMultiaddrsMock struct {
	local, remote ma.Multiaddr
}

func (c *connMultiaddrsMock) LocalMultiaddr() ma.Multiaddr  { return c.local }
func (c *connMultiaddrsMock) RemoteMultiaddr() ma.Multiaddr { return c.remote }

func TestHost(t *testing.T) {
	// ARRANGE
	ctx := context.Background()

	// Given sample protocol ids
	var (
		channelFoo = byte(0xaa)
		channelBar = byte(0xbb)
		protoFoo   = ProtocolID(channelFoo)
		protoBar   = ProtocolID(channelBar)
	)

	// Given 2 available ports
	ports := utils.GetFreePorts(t, 2)

	// Given two hosts that are connected to each other
	host1 := makeTestHost(t, ports[0], withLogging())
	host2 := makeTestHost(t, ports[1], withLogging(), withBootstrapPeers([]config.LibP2PBootstrapPeer{
		{
			// resolve host via hostname
			Host: fmt.Sprintf("localhost:%d", ports[0]),
			ID:   host1.ID().String(),
		},
	}))

	connectBootstrapPeers(t, ctx, host2, host2.BootstrapPeers())

	t.Logf("host1: %+v", host1.AddrInfo())
	t.Logf("host2: %+v", host2.AddrInfo())

	// Given sample envelope
	type envelope struct {
		protocol protocol.ID
		sender   peer.ID
		receiver peer.ID
		message  string
	}

	envelopes := []envelope{}
	mu := sync.Mutex{}

	// Given sample handler for both hosts
	handler := func(stream network.Stream) {
		var (
			conn     = stream.Conn()
			receiver = conn.LocalPeer()
			sender   = conn.RemotePeer()
		)

		if conn.ConnState().Transport != TransportQUIC {
			t.Fatalf("unexpected transport: %s", conn.ConnState().Transport)
			return
		}

		payload, err := StreamReadClose(stream)
		if err != nil {
			t.Fatalf("failed to read from stream originating from %s: %v", sender, err)
			return
		}

		msg := &types.Request{}
		require.NoError(t, msg.Unmarshal(payload))
		require.NotNil(t, msg.GetEcho())

		e := envelope{
			protocol: stream.Protocol(),
			sender:   sender,
			receiver: receiver,
			message:  msg.GetEcho().GetMessage(),
		}

		logMessage := e.message
		if len(logMessage) > 64 {
			logMessage = logMessage[:64] + "..."
		}

		t.Logf(
			"Received envelope: %s -> %s (proto %s): %s",
			e.sender.String(),
			e.receiver.String(),
			e.protocol,
			logMessage,
		)

		mu.Lock()
		defer mu.Unlock()

		envelopes = append(envelopes, e)
	}

	host1.SetStreamHandler(protoFoo, handler)
	host1.SetStreamHandler(protoBar, handler)

	host2.SetStreamHandler(protoFoo, handler)
	host2.SetStreamHandler(protoBar, handler)

	// Given counter peers
	host1Peer2, err := NewPeer(host1, host2.AddrInfo(), p2p.NopMetrics(), false, false, false)
	require.NoError(t, err, "failed to create peer 1->2")
	require.NoError(t, host1Peer2.Start(), "failed to start peer 1->2")

	host2Peer1, err := NewPeer(host2, host1.AddrInfo(), p2p.NopMetrics(), false, false, false)
	require.NoError(t, err, "failed to create peer 2->1")
	require.NoError(t, host2Peer1.Start(), "failed to start peer 2->1")

	t.Logf("host1Peer2: %+v", host1Peer2.ID())
	t.Logf("host2Peer1: %+v", host2Peer1.ID())

	// Given a long string
	// 300kb
	longStr := strings.Repeat("a", 300*1024)

	// ACT
	send1 := host1Peer2.Send(p2p.Envelope{
		ChannelID: channelFoo,
		Message:   types.ToRequestEcho("one two"),
	})

	send2 := host2Peer1.Send(p2p.Envelope{
		ChannelID: channelBar,
		Message:   types.ToRequestEcho("three four"),
	})

	send3 := host1Peer2.TrySend(p2p.Envelope{
		ChannelID: channelBar,
		Message:   types.ToRequestEcho(longStr),
	})

	// ASSERT
	// Ensure we've written to both streams
	require.True(t, send1, "failed to send message 1->2")
	require.True(t, send2, "failed to send message 2->1")
	require.True(t, send3, "failed to send message 1->2")

	// Ensure two envelopes have been received
	wait := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(envelopes) == 3
	}

	require.Eventually(t, wait, 500*time.Millisecond, 50*time.Millisecond)

	// Ensure both envelopes match the expected ones
	expectedEnvelopes := []envelope{
		{
			protocol: protoFoo,
			sender:   host1.ID(),
			receiver: host2.ID(),
			message:  "one two",
		},
		{
			protocol: protoBar,
			sender:   host2.ID(),
			receiver: host1.ID(),
			message:  "three four",
		},
		{
			protocol: protoBar,
			sender:   host1.ID(),
			receiver: host2.ID(),
			message:  longStr,
		},
	}

	require.ElementsMatch(t, expectedEnvelopes, envelopes)

	t.Run("Ping", func(t *testing.T) {
		// ARRANGE
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// ACT
		rtt, err := host1.Ping(ctx, host2.AddrInfo())

		// ASSERT
		require.NoError(t, err)
		require.NotZero(t, rtt)

		t.Logf("host1 -> host2 RTT: %s", rtt.String())
	})
}

func TestHostConnGater(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		// ARRANGE
		cfg := config.DefaultP2PConfig()
		cfg.LibP2PConfig.Limits.Mode = config.LibP2PLimitsModeDefault

		// ACT
		connGater, enabled := ConnectionGaterFromConfig(cfg, nil)

		// ASSERT
		require.Nil(t, connGater)
		require.False(t, enabled)
	})

	t.Run("disabled", func(t *testing.T) {
		// ARRANGE
		cfg := config.DefaultP2PConfig()
		cfg.LibP2PConfig.Limits.Mode = config.LibP2PLimitsModeDisabled

		// ACT
		connGater, enabled := ConnectionGaterFromConfig(cfg, nil)

		// ASSERT
		require.Nil(t, connGater)
		require.False(t, enabled)
	})

	t.Run("peer exchange", func(t *testing.T) {
		// ARRANGE
		cfg := config.DefaultP2PConfig()
		cfg.LibP2PConfig.PEX.Enabled = true

		// ACT
		connGater, enabled := ConnectionGaterFromConfig(cfg, nil)

		// ASSERT
		require.True(t, enabled)
		require.Zero(t, connGater.maxPeers)
		require.True(t, connGater.limitInbound)
		require.Equal(t, cfg.MaxNumInboundPeers, connGater.maxInbound)
	})

	t.Run("custom limits without peer exchange", func(t *testing.T) {
		// ARRANGE
		cfg := config.DefaultP2PConfig()
		cfg.LibP2PConfig.Limits.Mode = config.LibP2PLimitsModeCustom
		cfg.LibP2PConfig.Limits.MaxPeers = 10

		// ACT
		connGater, enabled := ConnectionGaterFromConfig(cfg, nil)

		// ASSERT
		require.True(t, enabled)
		require.Equal(t, 10, connGater.maxPeers)
		require.False(t, connGater.limitInbound)
	})

	t.Run("custom limits with peer exchange", func(t *testing.T) {
		// ARRANGE
		cfg := config.DefaultP2PConfig()
		cfg.LibP2PConfig.Limits.Mode = config.LibP2PLimitsModeCustom
		cfg.LibP2PConfig.Limits.MaxPeers = 100
		cfg.LibP2PConfig.PEX.Enabled = true

		// ACT
		connGater, enabled := ConnectionGaterFromConfig(cfg, nil)

		// ASSERT
		require.True(t, enabled)
		require.Equal(t, 100, connGater.maxPeers)
		require.True(t, connGater.limitInbound)
		require.Equal(t, cfg.MaxNumInboundPeers, connGater.maxInbound)
	})

	t.Run("inbound cap", func(t *testing.T) {
		for _, tc := range []struct {
			name           string
			maxInbound     int
			firstBootstrap bool
			firstOutbound  bool
			extraBootstrap bool
			connected      bool
		}{
			{name: "rejects inbound peer beyond the cap", maxInbound: 1},
			{name: "admits inbound bootstrap peer beyond the cap", maxInbound: 1, extraBootstrap: true, connected: true},
			{name: "outbound peers take no inbound slot", maxInbound: 1, firstOutbound: true, connected: true},
			{name: "bootstrap peers take no inbound slot", maxInbound: 1, firstBootstrap: true, connected: true},
			{name: "zero cap rejects inbound peers", firstOutbound: true},
			{name: "zero cap admits inbound bootstrap peers", firstOutbound: true, extraBootstrap: true, connected: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				// ARRANGE
				ports := utils.GetFreePorts(t, 3)
				first, extra := makeTestHost(t, ports[1]), makeTestHost(t, ports[2])

				var bootstrapPeers []config.LibP2PBootstrapPeer
				if tc.firstBootstrap {
					bootstrapPeers = append(bootstrapPeers, config.LibP2PBootstrapPeer{Host: fmt.Sprintf("127.0.0.1:%d", ports[1]), ID: first.ID().String()})
				}
				if tc.extraBootstrap {
					bootstrapPeers = append(bootstrapPeers, config.LibP2PBootstrapPeer{Host: fmt.Sprintf("127.0.0.1:%d", ports[2]), ID: extra.ID().String()})
				}
				h := makeTestHost(t, ports[0], withBootstrapPeers(bootstrapPeers), withMaxInboundPeers(tc.maxInbound), withModifiedConfig(func(cfg *config.LibP2PConfig) {
					cfg.PEX.Enabled = true
				}))
				if tc.firstOutbound {
					require.NoError(t, h.Connect(t.Context(), first.AddrInfo()))
				} else {
					require.NoError(t, first.Connect(t.Context(), h.AddrInfo()))
				}
				require.Eventually(t, func() bool { return len(h.Network().Peers()) == 1 }, 2*time.Second, 50*time.Millisecond)

				// ACT
				_ = extra.Connect(t.Context(), h.AddrInfo())

				// ASSERT
				isConnected := func() bool { return h.Network().Connectedness(extra.ID()) == network.Connected }
				if tc.connected {
					require.Eventually(t, isConnected, 2*time.Second, 50*time.Millisecond)
					return
				}
				require.Never(t, isConnected, 500*time.Millisecond, 50*time.Millisecond)
			})
		}
	})

	t.Run("bans", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			banned    bool
			bootstrap bool
			outbound  bool
		}{
			{name: "rejects banned inbound peer", banned: true},
			{name: "rejects banned outbound peer", banned: true, outbound: true},
			{name: "admits banned inbound bootstrap peer", banned: true, bootstrap: true},
			{name: "admits banned outbound bootstrap peer", banned: true, bootstrap: true, outbound: true},
			{name: "admits peer that is not banned"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ports := utils.GetFreePorts(t, 2)
				other := makeTestHost(t, ports[1])

				var bootstrapPeers []config.LibP2PBootstrapPeer
				if tc.bootstrap {
					bootstrapPeers = []config.LibP2PBootstrapPeer{{
						Host: fmt.Sprintf("127.0.0.1:%d", ports[1]),
						ID:   other.ID().String(),
					}}
				}
				h := makeTestHost(t, ports[0], withBootstrapPeers(bootstrapPeers), withModifiedConfig(func(cfg *config.LibP2PConfig) {
					cfg.PEX.Enabled = true
				}))
				var bans banList
				if tc.banned {
					bans = banList{other.ID()}
				}
				h.SetBanChecker(bans)
				connected := !tc.banned || tc.bootstrap

				if tc.outbound {
					err := h.Connect(t.Context(), other.AddrInfo())
					if !connected {
						require.ErrorIs(t, err, swarm.ErrGaterDisallowedConnection)
						return
					}
				} else {
					_ = other.Connect(t.Context(), h.AddrInfo())
				}

				isConnected := func() bool { return h.Network().Connectedness(other.ID()) == network.Connected }
				if connected {
					require.Eventually(t, isConnected, 2*time.Second, 50*time.Millisecond)
					return
				}
				require.Never(t, isConnected, 500*time.Millisecond, 50*time.Millisecond)
			})
		}
	})

	t.Run("maxPeers", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			bootstrap bool
			outbound  bool
			connected bool
		}{
			{name: "rejects inbound peer"},
			{name: "rejects outbound peer", outbound: true},
			{name: "admits inbound bootstrap peer", bootstrap: true, connected: true},
			{name: "admits outbound bootstrap peer", bootstrap: true, outbound: true, connected: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				// ARRANGE
				ctx := context.Background()
				ports := utils.GetFreePorts(t, 4)
				extra := makeTestHost(t, ports[3])

				var bootstrapPeers []config.LibP2PBootstrapPeer
				if tc.bootstrap {
					bootstrapPeers = []config.LibP2PBootstrapPeer{{
						Host: fmt.Sprintf("127.0.0.1:%d", ports[3]),
						ID:   extra.ID().String(),
					}}
				}

				// given host1 that is full with host2 and host3
				host1 := makeTestHost(t, ports[0], withBootstrapPeers(bootstrapPeers), withModifiedConfig(func(cfg *config.LibP2PConfig) {
					cfg.Limits.Mode = config.LibP2PLimitsModeCustom
					cfg.Limits.MaxPeers = 2
					cfg.Limits.MaxPeerStreams = 2
				}))
				for _, port := range ports[1:3] {
					require.NoError(t, makeTestHost(t, port).Connect(ctx, host1.AddrInfo()))
				}
				require.Eventually(t, func() bool { return len(host1.Network().Peers()) == 2 }, 2*time.Second, 50*time.Millisecond)

				// ACT
				// an inbound connect may return nil before host1 closes the connection
				if tc.outbound {
					_ = host1.Connect(ctx, extra.AddrInfo())
				} else {
					_ = extra.Connect(ctx, host1.AddrInfo())
				}

				// ASSERT
				isConnected := func() bool { return host1.Network().Connectedness(extra.ID()) == network.Connected }
				if tc.connected {
					require.Eventually(t, isConnected, 2*time.Second, 50*time.Millisecond)
					require.Len(t, host1.Network().Peers(), 3)
					return
				}
				require.Never(t, isConnected, 500*time.Millisecond, 50*time.Millisecond)
				require.Len(t, host1.Network().Peers(), 2)
			})
		}
	})

	t.Run("hostNotSet", func(t *testing.T) {
		bootstrapID, err := IDFromPrivateKey(ed25519.GenPrivKey())
		require.NoError(t, err)
		cg := &ConnGater{maxPeers: 10, bootstrapPeers: map[peer.ID]BootstrapPeer{bootstrapID: {}}}
		addr, err := ma.NewMultiaddr("/ip4/127.0.0.1/udp/26656/quic-v1")
		require.NoError(t, err)
		cm := &connMultiaddrsMock{local: addr, remote: addr}

		require.False(t, cg.InterceptSecured(network.DirInbound, peer.ID(""), cm))
		require.False(t, cg.InterceptAddrDial(peer.ID(""), addr))
		require.False(t, cg.InterceptPeerDial(peer.ID("")))
		require.True(t, cg.InterceptSecured(network.DirInbound, bootstrapID, cm))
		require.True(t, cg.InterceptPeerDial(bootstrapID))
		require.True(t, cg.InterceptAddrDial(bootstrapID, addr))
		require.True(t, cg.InterceptAccept(cm))
		require.True(t, cg.InterceptSecured(network.DirOutbound, peer.ID(""), cm))
	})

	t.Run("setHostWhileIntercepting", func(t *testing.T) {
		cg := &ConnGater{maxPeers: 10}
		h := makeTestHosts(t, 1)[0]

		done := make(chan struct{})
		go func() {
			defer close(done)
			cg.SetHost(h)
		}()
		_ = cg.InterceptPeerDial(h.ID())
		<-done

		require.True(t, cg.InterceptPeerDial(h.ID()))
	})
}

func TestConnGaterInboundPeers(t *testing.T) {
	ids := make([]peer.ID, 3)
	for i := range ids {
		id, err := IDFromPrivateKey(ed25519.GenPrivKey())
		require.NoError(t, err)
		ids[i] = id
	}
	in := func(i int) network.Conn { return &dirConn{id: ids[i], dir: network.DirInbound} }
	out := func(i int) network.Conn { return &dirConn{id: ids[i], dir: network.DirOutbound} }

	for _, tc := range []struct {
		name     string
		conns    []network.Conn
		expected int
	}{
		{name: "no connections"},
		{name: "inbound peers", conns: []network.Conn{in(0), in(1)}, expected: 2},
		{name: "outbound peer", conns: []network.Conn{out(0)}},
		{name: "peer with two inbound connections", conns: []network.Conn{in(0), in(0)}, expected: 1},
		{name: "outbound then inbound connection", conns: []network.Conn{out(0), in(0)}},
		{name: "inbound then outbound connection", conns: []network.Conn{in(0), out(0)}},
		{name: "bootstrap peer", conns: []network.Conn{in(2)}},
		{name: "mixed", conns: []network.Conn{in(0), out(1), in(1), in(2)}, expected: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cg := &ConnGater{bootstrapPeers: map[peer.ID]BootstrapPeer{ids[2]: {}}}

			require.Equal(t, tc.expected, cg.inboundPeers(tc.conns))
		})
	}
}

func TestHostProtectsBootstrapPeers(t *testing.T) {
	ports := utils.GetFreePorts(t, 2)
	bootstrap := makeTestHost(t, ports[1])

	h := makeTestHost(t, ports[0], withBootstrapPeers([]config.LibP2PBootstrapPeer{{
		Host: fmt.Sprintf("127.0.0.1:%d", ports[1]),
		ID:   bootstrap.ID().String(),
	}}))

	require.True(t, h.ConnManager().IsProtected(bootstrap.ID(), connMgrBootstrapTag))
	other, err := IDFromPrivateKey(ed25519.GenPrivKey())
	require.NoError(t, err)
	require.False(t, h.ConnManager().IsProtected(other, ""))
}

func TestResourceManager(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		// ARRANGE
		cfg := config.DefaultP2PConfig().LibP2PConfig
		cfg.Limits.Mode = config.LibP2PLimitsModeDisabled

		// ACT
		rm, _, err := ResourceManagerFromConfig(cfg)

		// ASSERT
		require.NoError(t, err)
		_, ok := rm.(*network.NullResourceManager)
		require.True(t, ok)
	})

	t.Run("default", func(t *testing.T) {
		// ARRANGE
		cfg := config.DefaultP2PConfig().LibP2PConfig
		cfg.Limits.Mode = config.LibP2PLimitsModeDefault

		// ACT
		rm, limiter, err := ResourceManagerFromConfig(cfg)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, rm.Close()) })

		// ASSERT
		require.NotNil(t, rm)
		_, isNull := rm.(*network.NullResourceManager)
		require.False(t, isNull)

		// there should be some limits base on defaults and some params related to the current machine
		systemLimits := limiter.GetSystemLimits()
		peerLimits := limiter.GetPeerLimits("")
		peerPingLimits := limiter.GetProtocolPeerLimits(ping.ID)
		t.Logf("systemLimits: %T: %+v", systemLimits, systemLimits)
		t.Logf("peerLimits: %T: %+v", peerLimits, peerLimits)
		t.Logf("peerPingLimits: %T: %+v", peerPingLimits, peerPingLimits)

		require.Less(t, systemLimits.GetStreamTotalLimit(), 1_000_000)
		require.Less(t, peerLimits.GetStreamTotalLimit(), systemLimits.GetStreamTotalLimit())
		require.Equal(t, 2, peerPingLimits.GetStreamLimit(network.DirInbound))
		requirePEXLimits(t, limiter)
	})

	t.Run("custom", func(t *testing.T) {
		// ARRANGE
		cfg := config.DefaultP2PConfig().LibP2PConfig
		cfg.Limits.Mode = config.LibP2PLimitsModeCustom
		cfg.Limits.MaxPeerStreams = 50
		cfg.Limits.MaxPeers = 10

		// ACT
		rm, limiter, err := ResourceManagerFromConfig(cfg)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, rm.Close()) })

		// ASSERT
		require.NotNil(t, rm)
		_, isNull := rm.(*network.NullResourceManager)
		require.False(t, isNull)

		var (
			systemLimits   = limiter.GetSystemLimits()
			peerLimits     = limiter.GetPeerLimits("")
			serviceLimits  = limiter.GetServiceLimits(identify.ServiceName)
			peerPingLimits = limiter.GetProtocolPeerLimits(ping.ID)
		)

		t.Logf("systemLimits: %T: %+v", systemLimits, systemLimits)
		t.Logf("peerLimits: %T: %+v", peerLimits, peerLimits)
		t.Logf("serviceLimits(identify): %T: %+v", serviceLimits, serviceLimits)
		t.Logf("peerPingLimits: %T: %+v", peerPingLimits, peerPingLimits)

		// no stream limits on "system" scope...
		require.Equal(t, math.MaxInt64, systemLimits.GetStreamTotalLimit())

		// ...but system connections follow max_peers, not max_peer_streams
		require.Equal(t, cfg.Limits.MaxPeers*systemConnsPerPeer, systemLimits.GetConnTotalLimit())
		moreStreams := cfg
		moreStreams.Limits.MaxPeerStreams *= 10
		_, moreStreamsLimiter, err := ResourceManagerFromConfig(moreStreams)
		require.NoError(t, err)
		require.Equal(t, systemLimits.GetConnTotalLimit(), moreStreamsLimiter.GetSystemLimits().GetConnTotalLimit())

		// ...but strict limits on "peer" scope
		require.Equal(t, cfg.Limits.MaxPeerStreams, peerLimits.GetStreamTotalLimit())

		// and also default limits on built-in "service" scope
		require.NotEqual(t, math.MaxInt64, serviceLimits.GetStreamTotalLimit())
		require.Less(t, peerLimits.GetStreamTotalLimit(), systemLimits.GetStreamTotalLimit())
		require.Equal(t, 2, peerPingLimits.GetStreamLimit(network.DirInbound))
		requirePEXLimits(t, limiter)
	})

	t.Run("pex streams per peer", func(t *testing.T) {
		const attempts = PEXPeerStreams + 2
		for _, tc := range []struct {
			name     string
			hold     bool
			expected int32
		}{
			{name: "caps concurrent streams", hold: true, expected: PEXPeerStreams},
			{name: "admits sequential streams", expected: attempts},
		} {
			t.Run(tc.name, func(t *testing.T) {
				// ARRANGE
				ports := utils.GetFreePorts(t, 2)
				server := makeTestHost(t, ports[0])
				client := makeTestHost(t, ports[1], withModifiedConfig(func(cfg *config.LibP2PConfig) {
					cfg.Limits.Mode = config.LibP2PLimitsModeDisabled
				}))
				var handled atomic.Int32
				release := make(chan struct{})
				t.Cleanup(func() { close(release) })
				server.SetStreamHandler(ProtocolID(PEXChannel), func(s network.Stream) {
					handled.Add(1)
					if tc.hold {
						<-release
					}
					_ = s.Reset()
				})
				require.NoError(t, client.Connect(t.Context(), server.AddrInfo()))

				// ACT
				for i := range attempts {
					s, err := client.NewStream(t.Context(), server.ID(), ProtocolID(PEXChannel))
					if err == nil {
						_, _ = s.Write([]byte{0})
					}
					if !tc.hold {
						require.Eventually(t, func() bool { return handled.Load() > int32(i) }, 2*time.Second, 10*time.Millisecond)
					}
				}

				// ASSERT
				require.Eventually(t, func() bool { return handled.Load() >= tc.expected }, 2*time.Second, 20*time.Millisecond)
				require.Never(t, func() bool { return handled.Load() > tc.expected }, 300*time.Millisecond, 20*time.Millisecond)
			})
		}
	})
}

func requirePEXLimits(t *testing.T, limiter rcmgr.Limiter) {
	t.Helper()

	pexPeerLimits := limiter.GetProtocolPeerLimits(ProtocolID(PEXChannel))
	require.Equal(t, 4, pexPeerLimits.GetStreamLimit(network.DirInbound))
	require.Equal(t, 4, pexPeerLimits.GetStreamLimit(network.DirOutbound))
	require.Equal(t, 8, pexPeerLimits.GetStreamTotalLimit())
}

func TestBootstrapPeers(t *testing.T) {
	t.Run("valid config with peers", func(t *testing.T) {
		// ARRANGE
		pkID := func(pk ed25519.PrivKey) peer.ID {
			id, err := IDFromPrivateKey(pk)
			require.NoError(t, err)
			return id
		}

		// Given 2 private keys
		pk1 := ed25519.GenPrivKey()
		pk2 := ed25519.GenPrivKey()
		pkID1 := pkID(pk1)
		pkID2 := pkID(pk2)

		// Given a P2P config with libp2p enabled and address book peers
		cfg := config.DefaultP2PConfig()
		cfg.LibP2PConfig.BootstrapPeers = []config.LibP2PBootstrapPeer{
			{Host: "127.0.0.1:26656", ID: pkID1.String(), Private: true, Persistent: false, Unconditional: true},
			{Host: "127.0.0.1:26657", ID: pkID2.String(), Private: false, Persistent: true, Unconditional: false},
			// duplicate will be ignored
			{Host: "127.0.0.1:26657", ID: pkID2.String(), Private: false, Persistent: true, Unconditional: false},
		}

		// ACT
		bootstrapPeers, err := BootstrapPeersFromConfig(cfg.LibP2PConfig)

		// ASSERT
		require.NoError(t, err)
		require.Len(t, bootstrapPeers, 2)

		// Check first peer
		bp1, ok := bootstrapPeers[pkID1]
		require.True(t, ok)
		require.Equal(t, pkID1, bp1.AddrInfo.ID)
		require.Len(t, bp1.AddrInfo.Addrs, 1)
		require.True(t, bp1.Private)
		require.False(t, bp1.Persistent)
		require.True(t, bp1.Unconditional)

		// Check second peer
		bp2, ok := bootstrapPeers[pkID2]
		require.True(t, ok)
		require.Equal(t, pkID2, bp2.AddrInfo.ID)
		require.Len(t, bp2.AddrInfo.Addrs, 1)
		require.False(t, bp2.Private)
		require.True(t, bp2.Persistent)
		require.False(t, bp2.Unconditional)
	})

	t.Run("invalid host format", func(t *testing.T) {
		// ARRANGE
		cfg := config.DefaultP2PConfig()
		cfg.LibP2PConfig.BootstrapPeers = []config.LibP2PBootstrapPeer{
			{Host: "invalid-host", ID: "12D3KooWRqqKwyNnjwukrxXTUXLiNK838WN5tc8Nk2DnMVPbpVPV"},
		}

		// ACT
		bootstrapPeers, err := BootstrapPeersFromConfig(cfg.LibP2PConfig)

		// ASSERT
		require.Error(t, err)
		require.Nil(t, bootstrapPeers)
	})

	t.Run("invalid peer ID", func(t *testing.T) {
		// ARRANGE
		cfg := config.DefaultP2PConfig()
		cfg.LibP2PConfig.BootstrapPeers = []config.LibP2PBootstrapPeer{
			{Host: "127.0.0.1:26656", ID: "invalid-id"},
		}

		// ACT
		bootstrapPeers, err := BootstrapPeersFromConfig(cfg.LibP2PConfig)

		// ASSERT
		require.Error(t, err)
		require.Nil(t, bootstrapPeers)
	})
}

type testOpts struct {
	pk             ed25519.PrivKey
	bootstrapPeers []config.LibP2PBootstrapPeer
	enableLogging  bool
	modifyConfig   func(*config.LibP2PConfig)
	maxInbound     *int
}

type testOption func(*testOpts)

func withLogging() testOption {
	return func(opts *testOpts) { opts.enableLogging = true }
}

func withBootstrapPeers(bootstrapPeers []config.LibP2PBootstrapPeer) testOption {
	return func(opts *testOpts) { opts.bootstrapPeers = bootstrapPeers }
}

func withPrivateKey(pk ed25519.PrivKey) testOption {
	return func(opts *testOpts) { opts.pk = pk }
}

func withModifiedConfig(modifier func(*config.LibP2PConfig)) testOption {
	return func(opts *testOpts) { opts.modifyConfig = modifier }
}

func withMaxInboundPeers(n int) testOption {
	return func(opts *testOpts) { opts.maxInbound = &n }
}

func makeTestHost(t *testing.T, port int, opts ...testOption) *Host {
	t.Helper()

	optsVal := &testOpts{
		pk:             ed25519.GenPrivKey(),
		bootstrapPeers: []config.LibP2PBootstrapPeer{},
		enableLogging:  false,
	}

	for _, opt := range opts {
		opt(optsVal)
	}

	// config
	cfg := config.DefaultP2PConfig()
	cfg.RootDir = t.TempDir()
	cfg.ListenAddress = fmt.Sprintf("127.0.0.1:%d", port)
	cfg.ExternalAddress = fmt.Sprintf("127.0.0.1:%d", port)

	cfg.LibP2PConfig.Enabled = true
	cfg.LibP2PConfig.BootstrapPeers = optsVal.bootstrapPeers
	if optsVal.maxInbound != nil {
		cfg.MaxNumInboundPeers = *optsVal.maxInbound
	}
	if optsVal.modifyConfig != nil {
		optsVal.modifyConfig(&cfg.LibP2PConfig)
	}

	logger := log.NewNopLogger()
	if optsVal.enableLogging {
		logger = log.TestingLogger()
	}

	host, err := NewHost(cfg, optsVal.pk, logger)
	require.NoError(t, err)

	t.Cleanup(func() { host.Close() })

	return host
}

func connectBootstrapPeers(t *testing.T, ctx context.Context, h *Host, peers map[peer.ID]BootstrapPeer) {
	require.NotEmpty(t, peers, "no peers to connect to")

	for _, peer := range peers {
		// dial to self
		if h.ID().String() == peer.AddrInfo.ID.String() {
			continue
		}

		h.logger.Info("Connecting to peer", "peer_id", peer.AddrInfo.ID.String())

		err := h.Connect(ctx, peer.AddrInfo)
		require.NoError(t, err, "failed to connect to peer", "peer_id", peer.AddrInfo.ID.String())
	}
}

func makeTestHosts(t *testing.T, numHosts int, opts ...testOption) []*Host {
	ports := utils.GetFreePorts(t, numHosts)

	hosts := make([]*Host, len(ports))
	for i, port := range ports {
		hosts[i] = makeTestHost(t, port, opts...)
	}

	return hosts
}

type banList []peer.ID

func (b banList) IsBanned(id peer.ID) bool { return slices.Contains(b, id) }

type dirConn struct {
	network.Conn

	id  peer.ID
	dir network.Direction
}

func (c *dirConn) RemotePeer() peer.ID { return c.id }

func (c *dirConn) Stat() network.ConnStats {
	return network.ConnStats{Stats: network.Stats{Direction: c.dir}}
}
