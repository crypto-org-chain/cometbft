package lp2p

import (
	"bytes"
	"context"
	"errors"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/libs/log"
	"github.com/cometbft/cometbft/p2p"
	"github.com/cometbft/cometbft/p2p/conn"
	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	p2ptest "github.com/libp2p/go-libp2p/core/test"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	"github.com/stretchr/testify/suite"
)

type disconnectSuite struct {
	suite.Suite
}

func TestDisconnectSuite(t *testing.T) {
	suite.Run(t, new(disconnectSuite))
}

func (s *disconnectSuite) newSwitch(reconcile time.Duration, setup func(local, remote *Host)) (*Switch, *Host, *reactorMock, *syncBuffer) {
	hosts := makeTestHosts(s.T(), 2)
	if setup != nil {
		setup(hosts[0], hosts[1])
	}

	logs := &syncBuffer{}
	logger := log.NewTMLogger(logs)

	reactor := newReactorMock([]*conn.ChannelDescriptor{{
		ID:                  0xF6,
		Priority:            1,
		RecvMessageCapacity: 1024,
		MessageType:         &types.RequestEcho{},
	}}, logger)

	sw, err := NewSwitch(nil, hosts[0], []SwitchReactor{{Name: "mock", Reactor: reactor}}, p2p.NopMetrics(), logger)
	s.Require().NoError(err)
	sw.reconcileInterval = reconcile
	s.Require().NoError(sw.Start())
	s.T().Cleanup(func() { _ = sw.Stop() })

	s.Require().NoError(hosts[0].Connect(context.Background(), hosts[1].AddrInfo()))

	return sw, hosts[1], reactor, logs
}

func (s *disconnectSuite) TestPeerLeavesSet() {
	testCases := []struct {
		name            string
		bootstrap       bool
		persistent      bool
		remove          func(sw *Switch, p *Peer, remote *Host)
		expectReconnect bool
		noFlapDelay     bool
	}{
		{name: "connection closed: other peer", remove: closeFromRemote},
		{name: "connection closed: persistent bootstrap peer", bootstrap: true, persistent: true, remove: closeFromRemote, expectReconnect: true},
		{name: "connection closed: non-persistent bootstrap peer", bootstrap: true, remove: closeFromRemote, expectReconnect: true},
		{name: "dials failed: other peer", remove: dialsFailed},
		{name: "dials failed: non-persistent bootstrap peer", bootstrap: true, remove: dialsFailed, expectReconnect: true, noFlapDelay: true},
		{name: "no good addresses: non-persistent bootstrap peer", bootstrap: true, remove: noGoodAddresses, expectReconnect: true, noFlapDelay: true},
		{name: "misbehavior: non-persistent bootstrap peer", bootstrap: true, remove: misbehaved},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			sw, other, reactor, _ := s.newSwitch(time.Hour, func(local, remote *Host) {
				if tc.bootstrap {
					local.bootstrapPeers[remote.ID()] = BootstrapPeer{AddrInfo: remote.AddrInfo(), Persistent: tc.persistent}
				}
			})
			key := peerIDToKey(other.ID())

			if !tc.bootstrap {
				_, err := sw.peerSet.Add(other.AddrInfo(), reactorOpts(sw))
				s.Require().NoError(err)
			}

			s.Require().Eventually(func() bool { return len(reactor.peerEvents()) == 2 }, 5*time.Second, 20*time.Millisecond)
			p := sw.Peers().Get(key).(*Peer)
			s.Equal(tc.persistent, p.IsPersistent())

			if tc.noFlapDelay {
				sw.flaps = newFlapBackoff(time.Hour, time.Hour, time.Hour)
			}

			tc.remove(sw, p, other)

			s.Eventually(func() bool {
				events := reactor.peerEvents()
				return len(events) >= 3 && events[2] == "remove"
			}, 5*time.Second, 20*time.Millisecond)

			if !tc.expectReconnect {
				s.Never(func() bool { return sw.Peers().Get(key) != nil }, 300*time.Millisecond, 50*time.Millisecond)
				return
			}

			s.Eventually(func() bool { return sw.Peers().Get(key) != nil }, 5*time.Second, 20*time.Millisecond)
			s.True(sw.Peers().Get(key).IsOutbound())
		})
	}
}

func closeFromRemote(sw *Switch, _ *Peer, remote *Host) {
	_ = remote.Network().ClosePeer(sw.host.ID())
}

func dialsFailed(sw *Switch, p *Peer, _ *Host) {
	sw.StopPeerForError(p, swarm.ErrAllDialsFailed)
}

func noGoodAddresses(sw *Switch, p *Peer, _ *Host) {
	sw.StopPeerForError(p, swarm.ErrNoGoodAddresses)
}

func misbehaved(sw *Switch, p *Peer, _ *Host) {
	sw.StopPeerForError(p, errors.New("bad message"))
}

func goroutineStacks() string {
	var buf bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&buf, 1)
	return buf.String()
}

func (s *disconnectSuite) TestConnectionReopenedDuringRemovalStaysOpen() {
	sw, other, reactor, _ := s.newSwitch(time.Hour, nil)

	reactor.onRemovePeer = func(p2p.Peer) {
		_ = other.Connect(context.Background(), sw.host.AddrInfo())
	}

	_, err := sw.peerSet.Add(other.AddrInfo(), reactorOpts(sw))
	s.Require().NoError(err)

	s.Require().NoError(other.Network().ClosePeer(sw.host.ID()))

	s.Eventually(func() bool { return len(reactor.peerEvents()) == 3 }, 5*time.Second, 20*time.Millisecond)
	s.Never(func() bool {
		return sw.host.Network().Connectedness(other.ID()) != network.Connected
	}, 300*time.Millisecond, 50*time.Millisecond)
}

func (s *disconnectSuite) TestReconcileSkipsConnectedPeers() {
	sw, other, reactor, logs := s.newSwitch(50*time.Millisecond, nil)

	_, err := sw.peerSet.Add(other.AddrInfo(), reactorOpts(sw))
	s.Require().NoError(err)

	time.Sleep(300 * time.Millisecond)

	s.Equal([]string{"init", "add"}, reactor.peerEvents())
	s.NotContains(logs.String(), "Removing peer")
	s.NotContains(logs.String(), "Failed to remove peer")
}

func (s *disconnectSuite) TestStopStopsWatcher() {
	sw, other, _, _ := s.newSwitch(time.Hour, nil)

	_, err := sw.peerSet.Add(other.AddrInfo(), PeerAddOptions{})
	s.Require().NoError(err)

	sw.disconnects.add(other.ID())

	stopped := make(chan struct{})
	go func() {
		_ = sw.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		s.FailNow("switch did not stop")
	}

	s.Nil(sw.disconnectSub)
	s.NotPanics(sw.stopWatchingDisconnects)
	s.NotContains(goroutineStacks(), "drainConnectednessEvents")
	s.NotContains(goroutineStacks(), "processDisconnects")
}

func (s *disconnectSuite) TestStopWaitsForInFlightRemoval() {
	sw, other, reactor, _ := s.newSwitch(time.Hour, nil)

	var (
		mu    sync.Mutex
		order []string
	)
	record := func(step string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, step)
	}

	entered, release := make(chan struct{}), make(chan struct{})
	reactor.onRemovePeer = func(p2p.Peer) {
		close(entered)
		<-release
		record("removal finished")
	}
	reactor.onStop = func() { record("reactor stopped") }

	_, err := sw.peerSet.Add(other.AddrInfo(), reactorOpts(sw))
	s.Require().NoError(err)
	s.Require().NoError(other.Network().ClosePeer(sw.host.ID()))
	<-entered

	stopped := make(chan struct{})
	go func() {
		_ = sw.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		s.FailNow("stopped before removal finished")
	case <-time.After(300 * time.Millisecond):
	}

	close(release)

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		s.FailNow("switch did not stop")
	}

	mu.Lock()
	defer mu.Unlock()
	s.Equal([]string{"removal finished", "reactor stopped"}, order)
}

func (s *disconnectSuite) TestPeerAddedAfterItsConnectionClosedIsRemoved() {
	sw, other, _, _ := s.newSwitch(time.Hour, nil)

	s.Require().NoError(sw.disconnectSub.Close())
	s.Require().NoError(other.Network().ClosePeer(sw.host.ID()))
	s.Require().Eventually(func() bool {
		return sw.host.Network().Connectedness(other.ID()) != network.Connected
	}, 5*time.Second, 10*time.Millisecond)

	_, err := sw.addPeer(other.AddrInfo(), reactorOpts(sw))
	s.Require().NoError(err)

	s.Eventually(func() bool { return sw.Peers().Get(peerIDToKey(other.ID())) == nil }, 5*time.Second, 10*time.Millisecond)
}

func (s *disconnectSuite) TestStopAbandonsPendingReconnect() {
	testCases := []struct {
		name        string
		delay       time.Duration
		unreachable bool
	}{
		{name: "waiting out a delay", delay: time.Hour},
		{name: "backing off a failed dial", unreachable: true},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			sw, other, _, logs := s.newSwitch(time.Hour, nil)

			addrInfo := other.AddrInfo()
			if tc.unreachable {
				addrInfo = unreachablePeer(s.T())
			}

			sw.scheduleReconnect(addrInfo, PeerAddOptions{}, tc.delay)
			s.Require().Eventually(func() bool { return reconnectTaken(sw, addrInfo.ID) }, 5*time.Second, 10*time.Millisecond)
			if tc.unreachable {
				s.Require().Eventually(func() bool { return reconnectFailures(logs) == 1 }, 5*time.Second, 10*time.Millisecond)
			}

			s.Require().NoError(sw.Stop())

			s.Eventually(func() bool { return reconnectLoops(sw) == 0 }, 500*time.Millisecond, 20*time.Millisecond)
		})
	}
}

func (s *disconnectSuite) TestReconnectWakesTheRunningLoop() {
	sw, _, _, logs := s.newSwitch(time.Hour, nil)
	unreachable := unreachablePeer(s.T())
	failures := func(n int) func() bool {
		return func() bool { return reconnectFailures(logs) == n }
	}

	sw.scheduleReconnect(unreachable, PeerAddOptions{}, 0)
	s.Require().Eventually(failures(1), 5*time.Second, 10*time.Millisecond)

	sw.scheduleReconnect(unreachable, PeerAddOptions{}, 400*time.Millisecond)
	s.Require().Never(failures(2), 300*time.Millisecond, 10*time.Millisecond)
	s.Require().Eventually(failures(2), 500*time.Millisecond, 10*time.Millisecond)

	sw.scheduleReconnect(unreachable, PeerAddOptions{}, time.Hour)
	sw.scheduleReconnect(unreachable, PeerAddOptions{}, 0)
	s.Never(failures(3), 1500*time.Millisecond, 10*time.Millisecond)
	s.Equal(1, reconnectLoops(sw))
}

func (s *disconnectSuite) TestConcurrentReconnectsRunOneLoop() {
	sw, _, _, logs := s.newSwitch(time.Hour, nil)
	unreachable := unreachablePeer(s.T())

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { sw.scheduleReconnect(unreachable, PeerAddOptions{}, 300*time.Millisecond) })
	}
	wg.Wait()

	s.Require().Eventually(func() bool { return reconnectFailures(logs) > 0 }, 5*time.Second, 10*time.Millisecond)
	s.Require().Never(func() bool { return reconnectFailures(logs) > 1 }, 500*time.Millisecond, 10*time.Millisecond)
	s.Eventually(func() bool { return reconnectFailures(logs) == 2 }, 2*time.Second, 10*time.Millisecond)
	s.Equal(1, reconnectLoops(sw))
}

func (s *disconnectSuite) TestReconnectLoopTakesLatestRequestThenDeregisters() {
	sw, other, _, _ := s.newSwitch(time.Hour, nil)
	loop := newReconnectLoop()
	sw.reconnectMu.Lock()
	sw.reconnects[other.ID()] = loop
	sw.reconnectMu.Unlock()

	sw.scheduleReconnect(peer.AddrInfo{ID: other.ID()}, PeerAddOptions{}, time.Minute)
	sw.scheduleReconnect(other.AddrInfo(), PeerAddOptions{Persistent: true}, 0)
	s.Len(loop.wake, 1)

	req, delay, ok := sw.takeReconnect(other.ID(), loop)
	s.Require().True(ok)
	s.Equal(reconnectRequest{addrInfo: other.AddrInfo(), opts: PeerAddOptions{Persistent: true}}, req)
	s.InDelta(time.Minute, delay, float64(time.Second))
	s.Empty(loop.wake)

	_, _, ok = sw.takeReconnect(other.ID(), loop)
	s.False(ok)
	s.Zero(reconnectLoops(sw))
}

func (s *disconnectSuite) TestUnreachableBootstrapPeerIsRetried() {
	unreachable := unreachablePeer(s.T())
	_, _, _, logs := s.newSwitch(time.Hour, func(local, _ *Host) {
		local.bootstrapPeers[unreachable.ID] = BootstrapPeer{AddrInfo: unreachable}
	})

	s.Eventually(func() bool { return reconnectFailures(logs) > 0 }, 5*time.Second, 10*time.Millisecond)
}

func (s *disconnectSuite) TestReconnectToExistingPeerIsSilent() {
	sw, other, _, logs := s.newSwitch(time.Hour, nil)
	_, err := sw.addPeer(other.AddrInfo(), reactorOpts(sw))
	s.Require().NoError(err)

	sw.scheduleReconnect(other.AddrInfo(), PeerAddOptions{}, 0)

	s.Eventually(func() bool { return reconnectLoops(sw) == 0 }, 5*time.Second, 10*time.Millisecond)
	s.NotContains(logs.String(), "Reconnected to peer")
}

func reconnectLoops(sw *Switch) int {
	sw.reconnectMu.Lock()
	defer sw.reconnectMu.Unlock()

	return len(sw.reconnects)
}

func reconnectTaken(sw *Switch, id peer.ID) bool {
	sw.reconnectMu.Lock()
	defer sw.reconnectMu.Unlock()

	loop, ok := sw.reconnects[id]
	return ok && loop.pending == nil
}

func unreachablePeer(t testing.TB) peer.AddrInfo {
	return peer.AddrInfo{ID: p2ptest.RandPeerIDFatal(t)}
}

func reconnectFailures(logs *syncBuffer) int {
	return strings.Count(logs.String(), "Failed to reconnect to peer")
}

func (s *disconnectSuite) TestStopPeerHonorsCondition() {
	testCases := []struct {
		name          string
		removedBefore bool
	}{
		{name: "condition fails"},
		{name: "peer already removed", removedBefore: true},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			sw, other, reactor, logs := s.newSwitch(time.Hour, nil)

			p, err := sw.peerSet.Add(other.AddrInfo(), reactorOpts(sw))
			s.Require().NoError(err)

			expectedEvents := []string{"init", "add"}
			if tc.removedBefore {
				s.Require().NoError(sw.peerSet.Remove(p.ID(), PeerRemovalOptions{}))
			}

			sw.stopPeer(p, errConnectionClosed, func() bool { return tc.removedBefore })

			s.Equal(!tc.removedBefore, sw.Peers().Get(p.ID()) != nil)
			s.Equal(expectedEvents, reactor.peerEvents())
			s.NotContains(logs.String(), "Failed to remove peer")
			if !tc.removedBefore {
				s.NotContains(logs.String(), "Removing peer")
			}
		})
	}
}

func (s *disconnectSuite) TestClosedConnectionWhileStartingIsNotClosedAgain() {
	sw, other, reactor, _ := s.newSwitch(time.Hour, nil)

	opts := reactorOpts(sw)
	opts.OnBeforeStart = func(p *Peer) {
		sw.reactors.InitPeer(p)
		sw.stopPeer(p, errConnectionClosed, func() bool { return true })
	}

	_, err := sw.peerSet.Add(other.AddrInfo(), opts)
	s.Require().ErrorIs(err, errPeerRemovedDuringStart)

	s.Equal([]string{"init", "add", "remove"}, reactor.peerEvents())
	s.Equal(network.Connected, sw.host.Network().Connectedness(other.ID()))
}

func (s *disconnectSuite) TestFlapBackoff() {
	const base, maxDelay, stable = time.Second, 4 * time.Second, time.Minute

	type step struct {
		gap      time.Duration
		expected time.Duration
	}

	testCases := []struct {
		name  string
		steps []step
	}{
		{
			name:  "repeated flaps double up to the cap",
			steps: []step{{0, base}, {time.Second, 2 * base}, {time.Second, 4 * base}, {time.Second, maxDelay}},
		},
		{
			name:  "a drop within delay plus the stable window doubles",
			steps: []step{{0, base}, {time.Second, 2 * base}, {2*base + stable - time.Second, 4 * base}},
		},
		{
			name:  "a drop past delay plus the stable window resets",
			steps: []step{{0, base}, {time.Second, 2 * base}, {2*base + stable, base}},
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			now := time.Unix(0, 0)
			b := newFlapBackoff(base, maxDelay, stable)
			b.now = func() time.Time { return now }

			for _, st := range tc.steps {
				now = now.Add(st.gap)
				s.Equal(st.expected, b.next("peer"))
			}
		})
	}
}

func (s *disconnectSuite) TestDisconnectQueueDedupes() {
	q := newDisconnectQueue()

	for range 3 {
		q.add("peer")
	}
	q.add("other")

	s.Len(q.signal, 1)
	s.ElementsMatch([]peer.ID{"peer", "other"}, q.take())
	s.Empty(q.take())
}

func (s *disconnectSuite) TestPanicDuringRemovalKeepsConsumerRunning() {
	sw, other, reactor, _ := s.newSwitch(time.Hour, nil)

	var panicked atomic.Bool
	reactor.onRemovePeer = func(p2p.Peer) {
		if panicked.CompareAndSwap(false, true) {
			panic("boom")
		}
	}

	_, err := sw.peerSet.Add(other.AddrInfo(), reactorOpts(sw))
	s.Require().NoError(err)
	s.Require().NoError(other.Network().ClosePeer(sw.host.ID()))
	s.Require().Eventually(panicked.Load, 5*time.Second, 10*time.Millisecond)

	third := makeTestHosts(s.T(), 1)[0]
	s.Require().NoError(sw.host.Connect(context.Background(), third.AddrInfo()))
	_, err = sw.peerSet.Add(third.AddrInfo(), reactorOpts(sw))
	s.Require().NoError(err)
	s.Require().NoError(third.Network().ClosePeer(sw.host.ID()))

	s.Eventually(func() bool { return sw.Peers().Get(peerIDToKey(third.ID())) == nil }, 5*time.Second, 10*time.Millisecond)
}

func (s *disconnectSuite) TestReconcileRemovesPeerWithMissedEvent() {
	sw, other, _, _ := s.newSwitch(100*time.Millisecond, nil)
	key := peerIDToKey(other.ID())

	_, err := sw.peerSet.Add(other.AddrInfo(), reactorOpts(sw))
	s.Require().NoError(err)

	s.Require().NoError(sw.disconnectSub.Close())
	s.Require().NoError(other.Network().ClosePeer(sw.host.ID()))

	s.Eventually(func() bool { return sw.Peers().Get(key) == nil }, 5*time.Second, 20*time.Millisecond)
}

func (s *disconnectSuite) TestConnectednessEventFilter() {
	sw, other, _, _ := s.newSwitch(time.Hour, nil)

	_, err := sw.peerSet.Add(other.AddrInfo(), PeerAddOptions{})
	s.Require().NoError(err)

	sw.stopWatchingDisconnects()

	unknown := makeTestHosts(s.T(), 1)[0]

	testCases := []struct {
		name     string
		event    event.EvtPeerConnectednessChanged
		expected int
	}{
		{name: "known peer disconnected", event: event.EvtPeerConnectednessChanged{Peer: other.ID(), Connectedness: network.NotConnected}, expected: 1},
		{name: "known peer connected", event: event.EvtPeerConnectednessChanged{Peer: other.ID(), Connectedness: network.Connected}},
		{name: "unknown peer disconnected", event: event.EvtPeerConnectednessChanged{Peer: unknown.ID(), Connectedness: network.NotConnected}},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			sw.onConnectednessChanged(tc.event)
			s.Len(sw.disconnects.take(), tc.expected)
		})
	}
}

func (s *disconnectSuite) TestFlappingPeerReconnectsWithGrowingDelay() {
	const base = 200 * time.Millisecond

	sw, other, _, _ := s.newSwitch(time.Hour, func(local, remote *Host) {
		local.bootstrapPeers[remote.ID()] = BootstrapPeer{AddrInfo: remote.AddrInfo()}
	})
	sw.flaps = newFlapBackoff(base, time.Minute, time.Minute)
	key := peerIDToKey(other.ID())

	s.Require().Eventually(func() bool { return sw.Peers().Get(key) != nil }, 5*time.Second, 10*time.Millisecond)

	for _, minDelay := range []time.Duration{base, 2 * base} {
		closed := time.Now()
		s.Require().NoError(other.Network().ClosePeer(sw.host.ID()))

		s.Require().Eventually(func() bool { return sw.Peers().Get(key) == nil }, 5*time.Second, 5*time.Millisecond)
		s.Require().Eventually(func() bool { return sw.Peers().Get(key) != nil }, 5*time.Second, 5*time.Millisecond)
		s.GreaterOrEqual(time.Since(closed), minDelay)
	}
}

func (s *disconnectSuite) TestDrainDoesNotBlockOnSlowRemoval() {
	sw, other, reactor, _ := s.newSwitch(time.Hour, nil)

	var once sync.Once
	entered, release := make(chan struct{}), make(chan struct{})
	reactor.onRemovePeer = func(p2p.Peer) {
		once.Do(func() { close(entered) })
		<-release
	}
	s.T().Cleanup(func() { close(release) })

	third := makeTestHosts(s.T(), 1)[0]
	s.Require().NoError(sw.host.Connect(context.Background(), third.AddrInfo()))

	for _, h := range []*Host{other, third} {
		_, err := sw.peerSet.Add(h.AddrInfo(), reactorOpts(sw))
		s.Require().NoError(err)
	}

	s.Require().NoError(other.Network().ClosePeer(sw.host.ID()))
	<-entered
	s.Require().NoError(third.Network().ClosePeer(sw.host.ID()))

	emitter, err := sw.host.EventBus().Emitter(new(event.EvtPeerConnectednessChanged))
	s.Require().NoError(err)
	s.T().Cleanup(func() { _ = emitter.Close() })

	emitted := make(chan struct{})
	go func() {
		defer close(emitted)
		for range 64 {
			_ = emitter.Emit(event.EvtPeerConnectednessChanged{Peer: third.ID(), Connectedness: network.NotConnected})
		}
	}()

	select {
	case <-emitted:
	case <-time.After(5 * time.Second):
		s.Fail("event bus blocked behind a slow removal")
	}
}
