package lp2p

import (
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

type reconnectRequest struct {
	addrInfo peer.AddrInfo
	opts     PeerAddOptions
}

type reconnectLoop struct {
	wake      chan struct{}
	pending   *reconnectRequest
	notBefore time.Time
}

func newReconnectLoop() *reconnectLoop {
	return &reconnectLoop{wake: make(chan struct{}, 1)}
}

func (s *Switch) scheduleReconnect(addrInfo peer.AddrInfo, opts PeerAddOptions, delay time.Duration) {
	s.reconnectMu.Lock()
	defer s.reconnectMu.Unlock()

	loop, running := s.reconnects[addrInfo.ID]
	if !running {
		loop = newReconnectLoop()
		s.reconnects[addrInfo.ID] = loop

		go s.runReconnectLoop(addrInfo.ID, loop)
	}

	loop.pending = &reconnectRequest{addrInfo: addrInfo, opts: opts}
	if notBefore := time.Now().Add(delay); notBefore.After(loop.notBefore) {
		loop.notBefore = notBefore
	}

	select {
	case loop.wake <- struct{}{}:
	default:
	}
}

func (s *Switch) runReconnectLoop(id peer.ID, loop *reconnectLoop) {
	for {
		req, delay, ok := s.takeReconnect(id, loop)
		if !ok {
			return
		}

		if s.sleepUnlessWoken(delay, loop.wake) {
			s.reconnectPeer(req.addrInfo, MaxReconnectBackoff, req.opts, loop.wake)
		}
	}
}

func (s *Switch) takeReconnect(id peer.ID, loop *reconnectLoop) (reconnectRequest, time.Duration, bool) {
	s.reconnectMu.Lock()
	defer s.reconnectMu.Unlock()

	req := loop.pending
	if req == nil {
		delete(s.reconnects, id)
		return reconnectRequest{}, 0, false
	}

	loop.pending = nil
	select {
	case <-loop.wake:
	default:
	}

	return *req, time.Until(loop.notBefore), true
}

func (s *Switch) sleepUnlessWoken(d time.Duration, wake <-chan struct{}) bool {
	select {
	case <-time.After(d):
		return true
	case <-wake:
		return false
	case <-s.Quit():
		return false
	}
}
