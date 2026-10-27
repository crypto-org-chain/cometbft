package lp2p

import (
	"sort"
	"sync"

	"github.com/cometbft/cometbft/libs/log"
	cmtrand "github.com/cometbft/cometbft/libs/rand"
	"github.com/cometbft/cometbft/p2p"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/mr-tron/base58/base58"
	"github.com/pkg/errors"
)

// PeerSet represents a single entrypoint for managing Peer's lifecycle.
// Note that PeerSet DOESN'T manage networking (e.g. opening/closing connections).
type PeerSet struct {
	host *Host

	peers map[peer.ID]*peerEntry
	mu    sync.RWMutex

	metrics *p2p.Metrics
	logger  log.Logger
}

var _ p2p.IPeerSet = (*PeerSet)(nil)

var (
	ErrPeerExists = errors.New("peer already exists")
	ErrSelfPeer   = errors.New("peer is self")

	errPeerStarting           = errors.New("peer is starting; removal deferred")
	errRemovalSkipped         = errors.New("removal condition not met")
	errPeerNotFound           = errors.New("peer not found")
	errPeerRemovedDuringStart = errors.New("peer was removed while starting")
)

type peerEntry struct {
	peer     *Peer
	starting bool
	removal  *PeerRemovalOptions
	done     chan struct{}
}

type peerStoppingError struct {
	done <-chan struct{}
}

func (peerStoppingError) Error() string { return "peer is being removed" }

// NewPeerSet manager peers for a given switch
func NewPeerSet(host *Host, metrics *p2p.Metrics, logger log.Logger) *PeerSet {
	return &PeerSet{
		host:    host,
		peers:   make(map[peer.ID]*peerEntry),
		metrics: metrics,
		logger:  logger,
	}
}

func (ps *PeerSet) Has(key p2p.ID) bool {
	_, exists := ps.getByKey(key)

	return exists
}

func (ps *PeerSet) Get(key p2p.ID) p2p.Peer {
	peer, exists := ps.getByKey(key)
	if !exists {
		return nil
	}

	return peer
}

// PeerAddOptions options for adding a peer to the PeerSet.
// It includes behavioral flags and lifecycle callbacks for peer initialization.
type PeerAddOptions struct {
	Private       bool
	Persistent    bool
	Unconditional bool
	// Outbound is set only by Switch.addDialedPeer; it also marks an existing inbound peer outbound.
	Outbound      bool
	OnBeforeStart func(p *Peer)
	OnAfterStart  func(p *Peer)
	// OnStartFailed runs if the peer fails to start or a start callback panics (the panic is then re-raised).
	OnStartFailed func(p *Peer, reason any)
}

// Add adds a new peer to the peer set. Fails if:
// - peer is already present
// - peer is self
// - peer has no addresses
// - peer is still being removed (retry after the removal completes)
// - peer was removed while Add was running its callbacks
func (ps *PeerSet) Add(addrInfo peer.AddrInfo, opts PeerAddOptions) (*Peer, error) {
	id := addrInfo.ID

	switch {
	case id == ps.host.ID():
		return nil, ErrSelfPeer
	case len(addrInfo.Addrs) == 0:
		return nil, errors.New("peer has no addresses")
	}

	ps.logger.Info("Adding peer", "peer_id", id.String())

	p, err := NewPeer(ps.host, addrInfo, ps.metrics, opts.Private, opts.Persistent, opts.Unconditional)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create peer")
	}

	p.outbound.Store(opts.Outbound)

	e := &peerEntry{peer: p, starting: true, done: make(chan struct{})}
	if err := ps.insert(id, e); err != nil {
		return nil, err
	}

	var (
		settled bool
		failure any
	)
	defer func() {
		if !settled {
			ps.abortStart(id, e, opts.OnStartFailed, failure, recover())
		}
	}()

	if opts.OnBeforeStart != nil {
		opts.OnBeforeStart(p)
	}

	if err := p.Start(); err != nil {
		failure = err
		return nil, errors.Wrap(err, "unable to start peer")
	}

	if opts.OnAfterStart != nil {
		opts.OnAfterStart(p)
	}

	ps.mu.Lock()
	e.starting = false
	removal := e.removal
	ps.metrics.Peers.Add(1)
	ps.mu.Unlock()

	settled = true

	if removal != nil {
		ps.finishRemoval(id, e, *removal)
		return nil, errPeerRemovedDuringStart
	}

	return p, nil
}

// PeerRemovalOptions options for removing a peer from the PeerSet.
// If OnAfterStop is provided, it is called after the peer is stopped and before its slot is freed.
// Note that Reason is any due to backwards compatibility.
type PeerRemovalOptions struct {
	Reason any
	// Condition, if set, is checked under the set lock (so it must not call PeerSet methods); the peer is kept
	// when it returns false. It is not re-checked when a removal is deferred until Add finishes.
	Condition   func() bool
	OnAfterStop func(p *Peer, reason any)
}

// Remove stops the peer and runs opts.OnAfterStop before the peer's slot is freed, so the peer cannot be
// re-added until removal completes. If the peer is still being added, the removal is deferred and performed
// by Add once its callbacks finish (or dropped if the peer fails to start), and an error is returned.
// If opts.Condition returns false, the peer is kept and an error is returned.
func (ps *PeerSet) Remove(key p2p.ID, opts PeerRemovalOptions) error {
	id := ps.keyToPeerID(key)
	if id == "" {
		return errors.New("invalid peer key")
	}

	if id == ps.host.ID() {
		return ErrSelfPeer
	}

	ps.mu.Lock()
	e, ok := ps.peers[id]
	if !ok || e.removal != nil {
		ps.mu.Unlock()
		return errPeerNotFound
	}

	if opts.Condition != nil && !opts.Condition() {
		ps.mu.Unlock()
		return errRemovalSkipped
	}

	e.removal = &opts
	starting := e.starting
	ps.mu.Unlock()

	ps.logger.Info("Removing peer", "peer_id", id.String(), "reason", opts.Reason)

	if starting {
		return errPeerStarting
	}

	ps.finishRemoval(id, e, opts)

	return nil
}

func (ps *PeerSet) RemoveAll(opts PeerRemovalOptions) {
	peers := ps.Copy()

	for _, peer := range peers {
		id := peer.ID()
		if err := ps.Remove(id, opts); err != nil && !errors.Is(err, errPeerStarting) {
			ps.logger.Error("Failed to remove peer", "peer_id", id, "err", err)
		}
	}
}

func (ps *PeerSet) abortStart(id peer.ID, e *peerEntry, onFailed func(*Peer, any), failure, recovered any) {
	defer ps.release(id, e)

	ps.mu.Lock()
	if e.removal != nil {
		ps.logger.Debug("Dropping removal of a peer that failed to start", "peer_id", id.String())
	}
	e.removal = &PeerRemovalOptions{}
	ps.mu.Unlock()

	if recovered != nil {
		failure = recovered
	}

	if e.peer.IsRunning() {
		if err := e.peer.Stop(); err != nil {
			ps.logger.Error("Failed to stop peer", "peer_id", id.String(), "err", err)
		}
	}

	if onFailed != nil {
		onFailed(e.peer, failure)
	}

	if recovered != nil {
		panic(recovered)
	}
}

func (ps *PeerSet) finishRemoval(id peer.ID, e *peerEntry, opts PeerRemovalOptions) {
	defer ps.release(id, e)

	if err := e.peer.Stop(); err != nil {
		ps.logger.Error("Failed to stop peer", "peer_id", id.String(), "err", err)
	}

	if opts.OnAfterStop != nil {
		opts.OnAfterStop(e.peer, opts.Reason)
	}
}

func (ps *PeerSet) ForEach(fn func(p2p.Peer)) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	for _, e := range ps.peers {
		if e.removal == nil {
			fn(e.peer)
		}
	}
}

// Random returns a random peer from the PeerSet (or nil if no peers are present).
// This method is not expected to be called frequently as it has O(n log n) complexity.
//
// Deprecated: use only for backwards compatibility.
func (ps *PeerSet) Random() p2p.Peer {
	peers := ps.Copy()
	if len(peers) == 0 {
		return nil
	}

	idx := cmtrand.Intn(len(peers))

	return peers[idx]
}

// Copy returns a copy of the peers list.
//
// Deprecated: use only for backwards compatibility.
func (ps *PeerSet) Copy() []p2p.Peer {
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	results := make([]p2p.Peer, 0, len(ps.peers))
	for _, e := range ps.peers {
		if e.removal == nil {
			results = append(results, e.peer)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].ID() < results[j].ID()
	})

	return results
}

// Size returns the number of peers in the peerSet.
func (ps *PeerSet) Size() int {
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	size := 0
	for _, e := range ps.peers {
		if e.removal == nil {
			size++
		}
	}

	return size
}

func (ps *PeerSet) getByKey(key p2p.ID) (*Peer, bool) {
	id := ps.keyToPeerID(key)
	if id == "" {
		return nil, false
	}

	ps.mu.RLock()
	defer ps.mu.RUnlock()

	e, ok := ps.peers[id]
	if !ok || e.removal != nil {
		return nil, false
	}

	return e.peer, true
}

func (ps *PeerSet) insert(id peer.ID, e *peerEntry) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	existing, ok := ps.peers[id]
	switch {
	case !ok:
		ps.peers[id] = e
		return nil
	case existing.removal != nil:
		return peerStoppingError{done: existing.done}
	}

	if e.peer.IsOutbound() {
		existing.peer.outbound.Store(true)
	}

	return ErrPeerExists
}

func (ps *PeerSet) release(id peer.ID, e *peerEntry) {
	ps.mu.Lock()
	delete(ps.peers, id)
	if !e.starting {
		ps.metrics.Peers.Add(-1)
	}
	ps.mu.Unlock()

	close(e.done)
}

func (ps *PeerSet) keyToPeerID(key p2p.ID) peer.ID {
	if key == "" {
		return ""
	}

	b, err := base58.Decode(string(key))
	if err != nil {
		ps.logger.Error("Failed to decode base58 key", "peer_key", key, "err", err)
		return ""
	}

	id, err := peer.IDFromBytes(b)
	if err != nil {
		ps.logger.Error("Failed to convert bytes to peer ID", "peer_key", key, "err", err)
		return ""
	}

	return id
}

// note that peerID.String() is base58 encoded and
// raw peerID is string([]byte)!
func peerIDToKey(id peer.ID) p2p.ID {
	return p2p.ID(id.String())
}
