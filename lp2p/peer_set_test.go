package lp2p

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cometbft/cometbft/libs/log"
	"github.com/cometbft/cometbft/libs/service"
	"github.com/cometbft/cometbft/p2p"
	"github.com/go-kit/kit/metrics"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

func TestPeerSet(t *testing.T) {
	t.Run("CRUD", func(t *testing.T) {
		// ARRANGE
		ctx := context.Background()

		hosts := makeTestHosts(t, 2)
		hostA := hosts[0]
		hostB := hosts[1]

		// Connect hosts so they know each other's addresses
		err := hostA.Connect(ctx, hostB.AddrInfo())
		require.NoError(t, err)

		ps := NewPeerSet(hostA, p2p.NopMetrics(), log.NewNopLogger())
		peerBKey := peerIDToKey(hostB.ID())

		// ACT & ASSERT #1: has(false) -> get(nil)
		hasPeer := ps.Has(peerBKey)
		assert.False(t, hasPeer)

		gotPeer := ps.Get(peerBKey)
		assert.Nil(t, gotPeer)

		// ACT & ASSERT #2: add -> get(ok) -> has(ok)
		peerB, err := ps.Add(hostB.AddrInfo(), PeerAddOptions{
			Private:       true,
			Persistent:    true,
			Unconditional: true,
		})
		require.NoError(t, err)
		require.NotNil(t, peerB)

		// check flags
		require.True(t, peerB.IsPrivate())
		require.True(t, peerB.IsPersistent())
		require.True(t, peerB.IsUnconditional())

		hasPeer = ps.Has(peerBKey)
		assert.True(t, hasPeer)

		gotPeer = ps.Get(peerBKey)
		require.NotNil(t, gotPeer)
		assert.Equal(t, peerBKey, gotPeer.ID())

		// ACT #3: remove -> has(false) -> get(nil)
		err = ps.Remove(peerBKey, PeerRemovalOptions{Reason: "test removal"})

		// ASSERT
		require.NoError(t, err)

		hasPeer = ps.Has(peerBKey)
		assert.False(t, hasPeer)

		gotPeer = ps.Get(peerBKey)
		assert.Nil(t, gotPeer)
	})

	t.Run("Safeguards", func(t *testing.T) {
		t.Run("Peer has no addresses", func(t *testing.T) {
			// ARRANGE
			hosts := makeTestHosts(t, 2)
			hostA := hosts[0]
			hostB := hosts[1]

			ps := NewPeerSet(hostA, p2p.NopMetrics(), log.NewNopLogger())

			// ACT
			_, err := ps.Add(peer.AddrInfo{ID: hostB.ID()}, PeerAddOptions{})

			// ASSERT
			require.Error(t, err)
		})

		t.Run("Add existing", func(t *testing.T) {
			// ARRANGE
			ctx := context.Background()

			hosts := makeTestHosts(t, 2)
			hostA := hosts[0]
			hostB := hosts[1]

			err := hostA.Connect(ctx, hostB.AddrInfo())
			require.NoError(t, err)

			ps := NewPeerSet(hostA, p2p.NopMetrics(), log.NewNopLogger())

			// ACT
			_, err1 := ps.Add(hostB.AddrInfo(), PeerAddOptions{})
			_, err2 := ps.Add(hostB.AddrInfo(), PeerAddOptions{})

			// ASSERT
			require.NoError(t, err1)
			require.ErrorIs(t, err2, ErrPeerExists)
		})

		t.Run("Remove non-existing", func(t *testing.T) {
			// ARRANGE
			hosts := makeTestHosts(t, 2)
			hostA := hosts[0]
			hostB := hosts[1]

			ps := NewPeerSet(hostA, p2p.NopMetrics(), log.NewNopLogger())
			nonExistentKey := peerIDToKey(hostB.ID())

			// ACT
			err := ps.Remove(nonExistentKey, PeerRemovalOptions{Reason: "test"})

			// ASSERT
			require.ErrorContains(t, err, "peer not found")
		})
	})

	t.Run("ForEach", func(t *testing.T) {
		// ARRANGE
		ctx := context.Background()
		const peers = 6

		hosts := makeTestHosts(t, peers)
		hostA := hosts[0]

		// Given peer set for hostA
		ps := NewPeerSet(hostA, p2p.NopMetrics(), log.NewNopLogger())

		// Given connected hosts
		for i := 1; i < peers; i++ {
			err := hostA.Connect(ctx, hosts[i].AddrInfo())
			require.NoError(t, err)

			_, err = ps.Add(hosts[i].AddrInfo(), PeerAddOptions{})
			require.NoError(t, err)
		}

		// ACT
		// collect ids
		collectedIDs := make(map[p2p.ID]struct{})
		ps.ForEach(func(p p2p.Peer) {
			collectedIDs[p.ID()] = struct{}{}
		})

		// ASSERT
		assert.Equal(t, peers-1, ps.Size())
		assert.Equal(t, peers-1, len(collectedIDs))
	})

	t.Run("Backwards compatibility", func(t *testing.T) {
		t.Run("Random", func(t *testing.T) {
			// ARRANGE
			ctx := context.Background()

			hosts := makeTestHosts(t, 4)
			hostA := hosts[0]
			ps := NewPeerSet(hostA, p2p.NopMetrics(), log.NewNopLogger())

			// ACT & ASSERT #1: Random returns nil when empty
			randomPeer := ps.Random()
			assert.Nil(t, randomPeer)

			// ARRANGE #2: Add some peers
			for i := 1; i < 4; i++ {
				err := hostA.Connect(ctx, hosts[i].AddrInfo())
				require.NoError(t, err)

				_, err = ps.Add(hosts[i].AddrInfo(), PeerAddOptions{})
				require.NoError(t, err)
			}

			// ACT & ASSERT #2: Random returns a valid peer
			randomPeer = ps.Random()
			require.NotNil(t, randomPeer)
			require.Contains(
				t,
				[]peer.ID{hosts[1].ID(), hosts[2].ID(), hosts[3].ID()},
				ps.keyToPeerID(randomPeer.ID()),
			)
		})

		t.Run("Copy", func(t *testing.T) {
			// ARRANGE
			ctx := context.Background()

			hosts := makeTestHosts(t, 4)
			hostA := hosts[0]
			ps := NewPeerSet(hostA, p2p.NopMetrics(), log.NewNopLogger())

			expectedIDs := make([]p2p.ID, 0, 3)

			for i := 1; i < 4; i++ {
				err := hostA.Connect(ctx, hosts[i].AddrInfo())
				require.NoError(t, err)

				_, err = ps.Add(hosts[i].AddrInfo(), PeerAddOptions{})
				require.NoError(t, err)

				expectedIDs = append(expectedIDs, peerIDToKey(hosts[i].ID()))
			}

			// ACT
			copied := ps.Copy()

			// ASSERT
			require.Len(t, copied, len(expectedIDs))

			copiedIDs := make([]p2p.ID, len(copied))
			for i, p := range copied {
				copiedIDs[i] = p.ID()
			}

			// ensure copy is sorted
			for i := 1; i < len(copied); i++ {
				assert.True(t, copied[i-1].ID() < copied[i].ID())
			}

			assert.ElementsMatch(t, expectedIDs, copiedIDs)
		})
	})
}

type peerSetLifecycleSuite struct {
	suite.Suite

	ps    *PeerSet
	peers *testGauge
	other *Host
}

type testGauge struct {
	mu    sync.Mutex
	value float64
}

func (g *testGauge) With(...string) metrics.Gauge { return g }

func (g *testGauge) Set(v float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.value = v
}

func (g *testGauge) Add(delta float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.value += delta
}

func (g *testGauge) Value() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.value
}

func TestPeerSetLifecycleSuite(t *testing.T) {
	suite.Run(t, new(peerSetLifecycleSuite))
}

func (s *peerSetLifecycleSuite) SetupTest() {
	hosts := makeTestHosts(s.T(), 2)

	m := p2p.NopMetrics()
	s.peers = &testGauge{}
	m.Peers = s.peers

	s.ps = NewPeerSet(hosts[0], m, log.NewNopLogger())
	s.other = hosts[1]
}

func (s *peerSetLifecycleSuite) key() p2p.ID {
	return peerIDToKey(s.other.ID())
}

func (s *peerSetLifecycleSuite) TestRemoveFreesSlotAfterCallbacks() {
	_, err := s.ps.Add(s.other.AddrInfo(), PeerAddOptions{})
	s.Require().NoError(err)
	s.Equal(1.0, s.peers.Value())

	entered, release, removed := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		removed <- s.ps.Remove(s.key(), PeerRemovalOptions{OnAfterStop: func(*Peer, any) {
			close(entered)
			<-release
		}})
	}()
	<-entered

	s.Nil(s.ps.Get(s.key()))
	s.Zero(s.ps.Size())
	s.Empty(s.ps.Copy())
	s.Nil(s.ps.Random())
	s.ps.ForEach(func(p2p.Peer) { s.Fail("removing peer is visible to ForEach") })

	_, err = s.ps.Add(s.other.AddrInfo(), PeerAddOptions{})
	var stopping peerStoppingError
	s.ErrorAs(err, &stopping)

	close(release)
	s.Require().NoError(<-removed)
	s.Zero(s.peers.Value())

	_, err = s.ps.Add(s.other.AddrInfo(), PeerAddOptions{})
	s.NoError(err)
}

func (s *peerSetLifecycleSuite) TestDeferredRemoval() {
	testCases := []struct {
		name          string
		remove        func(p *Peer, record func(string)) error
		expectedOrder []string
	}{
		{
			name: "remove",
			remove: func(p *Peer, record func(string)) error {
				return s.ps.Remove(p.ID(), PeerRemovalOptions{OnAfterStop: func(*Peer, any) { record("removed") }})
			},
			expectedOrder: []string{"added", "removed"},
		},
		{
			name: "remove all",
			remove: func(*Peer, func(string)) error {
				s.ps.RemoveAll(PeerRemovalOptions{})
				return errPeerStarting
			},
			expectedOrder: []string{"added"},
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			s.SetupTest()

			var order []string
			record := func(step string) { order = append(order, step) }

			_, err := s.ps.Add(s.other.AddrInfo(), PeerAddOptions{
				OnBeforeStart: func(p *Peer) {
					s.ErrorIs(tc.remove(p, record), errPeerStarting)
					s.ErrorContains(s.ps.Remove(p.ID(), PeerRemovalOptions{}), "peer not found")
				},
				OnAfterStart: func(*Peer) { record("added") },
			})

			s.ErrorIs(err, errPeerRemovedDuringStart)
			s.Equal(tc.expectedOrder, order)
			s.Nil(s.ps.Get(s.key()))
			s.Zero(s.peers.Value())

			_, err = s.ps.Add(s.other.AddrInfo(), PeerAddOptions{})
			s.NoError(err)
		})
	}
}

func (s *peerSetLifecycleSuite) TestFailingStopStillRunsCallbacks() {
	p, err := s.ps.Add(s.other.AddrInfo(), PeerAddOptions{})
	s.Require().NoError(err)
	s.Require().NoError(p.Stop())

	var removed bool
	s.Require().NoError(s.ps.Remove(s.key(), PeerRemovalOptions{OnAfterStop: func(*Peer, any) { removed = true }}))

	s.True(removed)
	s.Nil(s.ps.Get(s.key()))
	s.Zero(s.peers.Value())
}

func (s *peerSetLifecycleSuite) TestStartFailure() {
	testCases := []struct {
		name          string
		removeOnStart bool
	}{
		{name: "frees the slot"},
		{name: "drops a deferred removal", removeOnStart: true},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			s.SetupTest()

			var (
				removed bool
				failed  any
			)

			_, err := s.ps.Add(s.other.AddrInfo(), PeerAddOptions{
				OnBeforeStart: func(p *Peer) {
					s.Require().NoError(p.Start())
					if tc.removeOnStart {
						s.ErrorIs(s.ps.Remove(p.ID(), PeerRemovalOptions{OnAfterStop: func(*Peer, any) { removed = true }}), errPeerStarting)
					}
				},
				OnStartFailed: func(_ *Peer, reason any) {
					failed = reason
					s.Nil(s.ps.Get(s.key()))
					_, err := s.ps.Add(s.other.AddrInfo(), PeerAddOptions{})
					s.Error(err)
				},
			})

			s.Error(err)
			s.ErrorIs(failed.(error), service.ErrAlreadyStarted)
			s.False(removed)
			s.Nil(s.ps.Get(s.key()))
			s.Zero(s.peers.Value())

			_, err = s.ps.Add(s.other.AddrInfo(), PeerAddOptions{})
			s.NoError(err)
		})
	}
}

func (s *peerSetLifecycleSuite) TestPanicInCallbackFreesSlot() {
	testCases := []struct {
		name         string
		opts         PeerAddOptions
		failedPanics bool
	}{
		{name: "before start", opts: PeerAddOptions{OnBeforeStart: func(*Peer) { panic("boom") }}},
		{name: "after start", opts: PeerAddOptions{OnAfterStart: func(*Peer) { panic("boom") }}},
		{name: "start failure callback panics too", opts: PeerAddOptions{OnAfterStart: func(*Peer) { panic("boom") }}, failedPanics: true},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			s.SetupTest()

			var (
				added  *Peer
				failed any
			)
			opts := tc.opts
			before := opts.OnBeforeStart
			opts.OnBeforeStart = func(p *Peer) {
				added = p
				if before != nil {
					before(p)
				}
			}
			opts.OnStartFailed = func(_ *Peer, reason any) {
				failed = reason
				if tc.failedPanics {
					panic("again")
				}
			}

			s.Panics(func() { _, _ = s.ps.Add(s.other.AddrInfo(), opts) })

			s.False(added.IsRunning())
			s.Equal("boom", failed)
			s.Nil(s.ps.Get(s.key()))

			_, err := s.ps.Add(s.other.AddrInfo(), PeerAddOptions{})
			s.NoError(err)
		})
	}
}

func (s *peerSetLifecycleSuite) TestPanicInRemovalCallbackFreesSlot() {
	panicking := PeerRemovalOptions{OnAfterStop: func(*Peer, any) { panic("boom") }}

	testCases := []struct {
		name   string
		remove func()
	}{
		{
			name: "live peer",
			remove: func() {
				_, err := s.ps.Add(s.other.AddrInfo(), PeerAddOptions{})
				s.Require().NoError(err)
				_ = s.ps.Remove(s.key(), panicking)
			},
		},
		{
			name: "deferred removal",
			remove: func() {
				_, _ = s.ps.Add(s.other.AddrInfo(), PeerAddOptions{
					OnBeforeStart: func(p *Peer) { _ = s.ps.Remove(p.ID(), panicking) },
				})
			},
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			s.SetupTest()

			s.Panics(tc.remove)

			s.Nil(s.ps.Get(s.key()))
			s.Zero(s.peers.Value())

			_, err := s.ps.Add(s.other.AddrInfo(), PeerAddOptions{})
			s.NoError(err)
		})
	}
}

func (s *peerSetLifecycleSuite) TestConcurrentChurn() {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		started  int
		finished int
		deadline = time.Now().Add(500 * time.Millisecond)
	)

	opts := PeerAddOptions{
		OnAfterStart: func(*Peer) {
			mu.Lock()
			started++
			mu.Unlock()
		},
	}
	removal := PeerRemovalOptions{OnAfterStop: func(*Peer, any) {
		mu.Lock()
		finished++
		mu.Unlock()
	}}

	for range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				_, _ = s.ps.Add(s.other.AddrInfo(), opts)
			}
		}()
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				_ = s.ps.Remove(s.key(), removal)
			}
		}()
	}
	wg.Wait()

	_ = s.ps.Remove(s.key(), removal)

	s.Nil(s.ps.Get(s.key()))
	s.Zero(s.peers.Value())
	s.Equal(started, finished)
	s.Positive(started)
}

func (s *peerSetLifecycleSuite) TestRemoveCondition() {
	testCases := []struct {
		name      string
		starting  bool
		condition bool
	}{
		{name: "live peer, condition holds", condition: true},
		{name: "live peer, condition fails"},
		{name: "starting peer, condition holds", starting: true, condition: true},
		{name: "starting peer, condition fails", starting: true},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			s.SetupTest()

			var removeErr error
			remove := func(id p2p.ID) error {
				return s.ps.Remove(id, PeerRemovalOptions{Condition: func() bool { return tc.condition }})
			}

			opts := PeerAddOptions{}
			if tc.starting {
				opts.OnBeforeStart = func(p *Peer) { removeErr = remove(p.ID()) }
			}

			_, addErr := s.ps.Add(s.other.AddrInfo(), opts)
			if !tc.starting {
				s.Require().NoError(addErr)
				removeErr = remove(s.key())
			}

			switch {
			case !tc.condition:
				s.ErrorIs(removeErr, errRemovalSkipped)
				s.NoError(addErr)
			case tc.starting:
				s.ErrorIs(removeErr, errPeerStarting)
				s.ErrorIs(addErr, errPeerRemovedDuringStart)
			default:
				s.NoError(removeErr)
			}

			s.Equal(!tc.condition, s.ps.Get(s.key()) != nil)
		})
	}
}
