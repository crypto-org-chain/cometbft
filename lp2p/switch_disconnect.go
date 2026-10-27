package lp2p

import (
	"sync"
	"time"

	"github.com/cometbft/cometbft/p2p"
	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/host/eventbus"
	"github.com/pkg/errors"
)

const defaultReconcileInterval = 30 * time.Second

var errConnectionClosed = errors.New("connection closed")

func isConnectionClosed(reason any) bool {
	err, ok := reason.(error)
	return ok && errors.Is(err, errConnectionClosed)
}

func (s *Switch) isDisconnected(id peer.ID) bool {
	return s.host.Network().Connectedness(id) != network.Connected
}

type disconnectQueue struct {
	mu      sync.Mutex
	pending map[peer.ID]struct{}
	signal  chan struct{}
}

func newDisconnectQueue() *disconnectQueue {
	return &disconnectQueue{
		pending: make(map[peer.ID]struct{}),
		signal:  make(chan struct{}, 1),
	}
}

func (q *disconnectQueue) add(id peer.ID) {
	q.mu.Lock()
	q.pending[id] = struct{}{}
	q.mu.Unlock()

	select {
	case q.signal <- struct{}{}:
	default:
	}
}

func (q *disconnectQueue) take() []peer.ID {
	q.mu.Lock()
	defer q.mu.Unlock()

	ids := make([]peer.ID, 0, len(q.pending))
	for id := range q.pending {
		ids = append(ids, id)
	}
	clear(q.pending)

	return ids
}

func (s *Switch) watchDisconnects() error {
	sub, err := s.host.EventBus().Subscribe(new(event.EvtPeerConnectednessChanged), eventbus.Name("lp2p-switch"))
	if err != nil {
		return errors.Wrap(err, "failed to subscribe to connectedness events")
	}

	s.disconnectSub = sub
	s.disconnectQuit = make(chan struct{})

	s.disconnectWorkers.Add(2)
	go s.drainConnectednessEvents(sub)
	go s.processDisconnects(s.disconnectQuit)

	return nil
}

func (s *Switch) stopWatchingDisconnects() {
	if s.disconnectSub == nil {
		return
	}

	close(s.disconnectQuit)
	_ = s.disconnectSub.Close()
	s.disconnectWorkers.Wait()
	s.disconnectSub = nil
}

func (s *Switch) drainConnectednessEvents(sub event.Subscription) {
	defer s.disconnectWorkers.Done()

	for evt := range sub.Out() {
		if e, ok := evt.(event.EvtPeerConnectednessChanged); ok {
			s.onConnectednessChanged(e)
		}
	}
}

func (s *Switch) onConnectednessChanged(e event.EvtPeerConnectednessChanged) {
	if e.Connectedness != network.Connected && s.peerSet.Has(peerIDToKey(e.Peer)) {
		s.disconnects.add(e.Peer)
	}
}

func (s *Switch) processDisconnects(quit <-chan struct{}) {
	defer s.disconnectWorkers.Done()

	ticker := time.NewTicker(s.reconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-quit:
			return
		case <-s.disconnects.signal:
		case <-ticker.C:
			s.peerSet.ForEach(func(p p2p.Peer) { s.disconnects.add(p.(*Peer).addrInfo.ID) })
		}

		for _, id := range s.disconnects.take() {
			select {
			case <-quit:
				return
			default:
			}

			s.removeIfDisconnectedSafely(id)
		}
	}
}

func (s *Switch) removeIfDisconnectedSafely(id peer.ID) {
	defer func() {
		if r := recover(); r != nil {
			s.Logger.Error("Panic while removing disconnected peer", "peer_id", id.String(), "panic", r)
		}
	}()

	s.removeIfDisconnected(id)
}

func (s *Switch) removeIfDisconnected(id peer.ID) {
	p, ok := s.peerSet.Get(peerIDToKey(id)).(*Peer)
	if !ok {
		return
	}

	s.stopPeer(p, errConnectionClosed, func() bool { return s.isDisconnected(id) })
}

type flapBackoff struct {
	base, max, stableAfter time.Duration
	now                    func() time.Time

	mu    sync.Mutex
	state map[peer.ID]flapState
}

type flapState struct {
	delay time.Duration
	at    time.Time
}

func newFlapBackoff(base, maxDelay, stableAfter time.Duration) *flapBackoff {
	return &flapBackoff{base: base, max: maxDelay, stableAfter: stableAfter, now: time.Now, state: make(map[peer.ID]flapState)}
}

func (b *flapBackoff) next(id peer.ID) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	delay := b.base

	if prev, ok := b.state[id]; ok && now.Sub(prev.at) < prev.delay+b.stableAfter {
		delay = min(2*prev.delay, b.max)
	}

	b.state[id] = flapState{delay: delay, at: now}

	return delay
}
