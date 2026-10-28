package pex

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/cometbft/cometbft/lp2p"
	"github.com/cometbft/cometbft/p2p"
	"github.com/cometbft/cometbft/p2p/conn"
	tmp2p "github.com/cometbft/cometbft/proto/tendermint/p2p"
	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/sec"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	ma "github.com/multiformats/go-multiaddr"
)

// Channel is the PEX channel, shared with the classic transport.
const Channel = lp2p.PEXChannel

const (
	maxAddrs = 128
	// maxReplySize allows 256 bytes per address; one encodes in about 110.
	maxReplySize = maxAddrs * 256

	// maxIDLen guards the quadratic base58 decode; real IDs are about 52 characters.
	// The book enforces the stricter raw size policy.
	maxIDLen = 128

	msgInterval    = 5 * time.Second
	msgBurst       = lp2p.PEXPeerStreams - 1 // one reply per request, plus one stream for our own request
	requestTimeout = 20 * time.Second
	askInterval    = 30 * time.Second
	saveInterval   = 2 * time.Minute
	banDuration    = 24 * time.Hour

	dialInterval      = 30 * time.Second
	groupDialInterval = time.Minute
	busyCooldown      = time.Minute
)

var errMalformed = errors.New("malformed pex message")

var (
	_ p2p.MsgBytesFilter = (*Reactor)(nil)
	_ lp2p.PeerMarker    = (*Reactor)(nil)
)

// Host is the part of the libp2p host the reactor needs.
type Host interface {
	Peerstore() peerstore.Peerstore
	EventBus() event.Bus
	Network() network.Network
}

type dialer interface {
	DialPeer(ctx context.Context, addrInfo peer.AddrInfo) error
}

// Reactor gossips peer addresses over libp2p, fills the address book and
// dials peers from it until the node has maxOutbound outbound peers.
type Reactor struct {
	p2p.BaseReactor

	book        *AddrBook
	host        Host
	dialer      dialer
	maxOutbound int
	now         func() time.Time

	askEvery  time.Duration
	saveEvery time.Duration
	dialEvery time.Duration

	mu         sync.Mutex
	peers      map[p2p.ID]*peerState
	dialing    int
	groupDials map[string]time.Time
	stopped    bool

	identifySub event.Subscription
	ctx         context.Context
	cancel      context.CancelFunc
	workers     sync.WaitGroup
}

type peerState struct {
	tokens    float64
	refilled  time.Time
	requested time.Time
	timedOut  bool
}

// NewReactor creates a reactor that stores what it learns in book.
func NewReactor(book *AddrBook, host Host, maxOutbound int) *Reactor {
	r := &Reactor{
		book:        book,
		host:        host,
		maxOutbound: maxOutbound,
		now:         time.Now,
		askEvery:    askInterval,
		saveEvery:   saveInterval,
		dialEvery:   dialInterval,
		peers:       make(map[p2p.ID]*peerState),
		groupDials:  make(map[string]time.Time),
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.BaseReactor = *p2p.NewBaseReactor("PEX", r)

	return r
}

// SetSwitch also picks up the switch's dialer, which OnStart requires.
func (r *Reactor) SetSwitch(sw p2p.Switcher) {
	r.BaseReactor.SetSwitch(sw)
	r.dialer, _ = sw.(dialer)
}

// OnStart loads the book and disconnects banned peers, then starts asking,
// dialing and verifying inbound addresses.
func (r *Reactor) OnStart() error {
	if r.dialer == nil {
		return errors.New("switch cannot dial peers")
	}

	if err := r.book.Load(); err != nil {
		r.Logger.Error("Failed to load address book, starting empty", "err", err)
	}

	// The host accepts connections before the bans are loaded.
	for _, id := range r.host.Network().Peers() {
		if r.book.IsBanned(id) {
			r.Logger.Info("Disconnecting banned peer", "peer_id", id)
			if err := r.host.Network().ClosePeer(id); err != nil {
				r.Logger.Error("Failed to disconnect banned peer", "peer_id", id, "err", err)
			}
		}
	}

	sub, err := r.host.EventBus().Subscribe(new(event.EvtPeerIdentificationCompleted))
	if err != nil {
		return fmt.Errorf("subscribe to identify events: %w", err)
	}

	r.identifySub = sub
	r.workers.Add(2)
	go r.verifyInboundAddrs()
	go r.tick()

	return nil
}

// OnStop stops the workers and saves the book.
func (r *Reactor) OnStop() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()

	r.cancel()
	_ = r.identifySub.Close()
	r.workers.Wait()

	if err := r.book.Save(); err != nil {
		r.Logger.Error("Failed to save address book", "err", err)
	}
}

func (r *Reactor) GetChannels() []*conn.ChannelDescriptor {
	return []*conn.ChannelDescriptor{{
		ID:                  Channel,
		Priority:            1,
		SendQueueCapacity:   10,
		RecvMessageCapacity: maxReplySize,
		MessageType:         &tmp2p.Message{},
	}}
}

// AddPeer asks a new outbound peer for addresses when the book needs more.
// A peer whose identify has not finished yet is asked on a later tick.
func (r *Reactor) AddPeer(p p2p.Peer) {
	if p.IsOutbound() && r.book.NeedMoreAddrs() && r.askable(p) {
		r.request(p)
	}
}

// RemovePeer forgets the peer's rate limit and pending request.
func (r *Reactor) RemovePeer(p p2p.Peer, _ any) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.peers, p.ID())
}

// FilterMsgBytes rate-limits every message per peer before it is decoded.
// Honest peers send a request and a reply per askInterval, a few more at connect.
func (r *Reactor) FilterMsgBytes(_ byte, p p2p.Peer, _ []byte) error {
	if r.allowMsg(p.ID()) {
		return nil
	}

	return lp2p.ErrDropMessage
}

func (r *Reactor) Receive(e p2p.Envelope) {
	switch msg := e.Message.(type) {
	case *tmp2p.PexRequest:
		r.respond(e.Src)
	case *tmp2p.PexAddrs:
		r.receiveAddrs(e.Src, msg.Addrs)
	default:
		r.Logger.Debug("Unexpected PEX message", "peer_id", e.Src.ID(), "type", fmt.Sprintf("%T", msg))
	}
}

// MarkPeerAsGood records that the peer helped consensus.
func (r *Reactor) MarkPeerAsGood(p p2p.Peer) {
	if id, err := peerID(p); err == nil {
		r.book.MarkGood(id)
	}
}

func (r *Reactor) respond(src p2p.Peer) {
	exclude, err := peerID(src)
	if err != nil {
		return
	}

	entries := r.book.Gossipable(exclude, maxAddrs)
	addrs := make([]tmp2p.NetAddress, 0, len(entries))
	for _, e := range entries {
		ip, port, err := ipPort(e.Addr)
		if err != nil {
			continue
		}
		addrs = append(addrs, tmp2p.NetAddress{ID: e.ID.String(), IP: ip.String(), Port: uint32(port)})
	}

	src.TrySend(p2p.Envelope{ChannelID: Channel, Message: &tmp2p.PexAddrs{Addrs: addrs}})
}

// receiveAddrs accepts a reply only to our own request. Malformed input bans
// the sender; addresses the book refuses are dropped.
func (r *Reactor) receiveAddrs(src p2p.Peer, addrs []tmp2p.NetAddress) {
	if !r.takeRequest(src.ID()) {
		r.Logger.Debug("Dropping unsolicited PEX addresses", "peer_id", src.ID())
		return
	}

	origin, err := peerID(src)
	if err != nil {
		return
	}

	if len(addrs) > maxAddrs {
		r.punish(src, origin, fmt.Errorf("%w: %d addresses", errMalformed, len(addrs)))
		return
	}

	type parsed struct {
		id   peer.ID
		addr ma.Multiaddr
	}
	entries := make([]parsed, 0, len(addrs))
	for _, na := range addrs {
		id, addr, err := parseNetAddress(na)
		if err != nil {
			r.punish(src, origin, fmt.Errorf("%w: %w", errMalformed, err))
			return
		}
		entries = append(entries, parsed{id: id, addr: addr})
	}

	for _, e := range entries {
		if err := r.book.AddGossiped(e.id, e.addr, origin); err != nil {
			r.Logger.Debug("Dropping PEX address", "peer_id", e.id, "addr", e.addr, "origin", origin, "err", err)
		}
	}
}

func (r *Reactor) punish(src p2p.Peer, id peer.ID, err error) {
	r.Logger.Info("Banning peer", "peer_id", id, "duration", banDuration, "err", err)
	r.book.Ban(id, r.now().Add(banDuration))
	r.Switch.StopPeerForError(src, err)
}

// request marks a request pending and sends it in the background, since lp2p
// sends block on stream setup.
func (r *Reactor) request(p p2p.Peer) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.stopped {
		return
	}
	st := r.state(p.ID())
	if !st.requested.IsZero() {
		return
	}
	st.requested = r.now()

	r.workers.Add(1)
	go func() {
		defer r.workers.Done()

		if !p.TrySend(p2p.Envelope{ChannelID: Channel, Message: &tmp2p.PexRequest{}}) {
			r.clearPending(p.ID())
		}
	}()
}

func (r *Reactor) takeRequest(id p2p.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	st, ok := r.peers[id]
	if !ok || st.requested.IsZero() {
		return false
	}
	st.requested, st.timedOut = time.Time{}, false

	return true
}

// clearPending drops a request that was never sent; only a reply clears timedOut.
func (r *Reactor) clearPending(id p2p.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if st, ok := r.peers[id]; ok {
		st.requested = time.Time{}
	}
}

func (r *Reactor) allowMsg(id p2p.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.state(id)
	now := r.now()
	st.tokens = min(msgBurst, st.tokens+now.Sub(st.refilled).Seconds()/msgInterval.Seconds())
	st.refilled = now

	if st.tokens < 1 {
		return false
	}
	st.tokens--

	return true
}

// state returns the peer's state, creating it with a full bucket. Callers hold r.mu.
func (r *Reactor) state(id p2p.ID) *peerState {
	st, ok := r.peers[id]
	if !ok {
		st = &peerState{tokens: msgBurst, refilled: r.now()}
		r.peers[id] = st
	}

	return st
}

func (r *Reactor) tick() {
	defer r.workers.Done()

	ask := time.NewTicker(r.askEvery)
	defer ask.Stop()
	save := time.NewTicker(r.saveEvery)
	defer save.Stop()
	dial := time.NewTimer(jitter(r.dialEvery))
	defer dial.Stop()

	for {
		select {
		case <-ask.C:
			r.askRandomPeer()
		case <-dial.C:
			r.dialPeers()
			dial.Reset(jitter(r.dialEvery))
		case <-save.C:
			if err := r.book.Save(); err != nil {
				r.Logger.Error("Failed to save address book", "err", err)
			}
		case <-r.ctx.Done():
			return
		}
	}
}

// askRandomPeer forgets departed peers and stale requests, then, while the book
// needs addresses, asks one random PEX peer without a pending request,
// preferring outbound peers whose last request did not time out.
func (r *Reactor) askRandomPeer() {
	var peers []p2p.Peer
	r.Switch.Peers().ForEach(func(p p2p.Peer) { peers = append(peers, p) })
	r.expire(peers)

	if !r.book.NeedMoreAddrs() {
		return
	}

	var preferred, others []p2p.Peer
	for _, p := range peers {
		if !r.askable(p) || r.hasRequest(p.ID()) {
			continue
		}
		if p.IsOutbound() && !r.timedOut(p.ID()) {
			preferred = append(preferred, p)
		} else {
			others = append(others, p)
		}
	}

	candidates := preferred
	if len(candidates) == 0 {
		candidates = others
	}
	if len(candidates) > 0 {
		r.request(candidates[rand.IntN(len(candidates))])
	}
}

// dialPeers dials book entries until outbound peers plus dials in flight
// reach maxOutbound, at most one dial per IP group per groupDialInterval.
// Candidates beyond need are requested so a skipped IP group does not cost a slot.
func (r *Reactor) dialPeers() {
	// Read dialing first: a dial finishing in between is then counted twice, never missed.
	r.mu.Lock()
	dialing := r.dialing
	r.mu.Unlock()
	outbound, _, _ := r.Switch.NumPeers()

	need := r.maxOutbound - outbound - dialing
	if need <= 0 {
		return
	}

	for _, e := range r.book.DialCandidates(3 * need) {
		if !r.reserveDial(e) {
			continue
		}
		go r.dial(e)
		if need--; need == 0 {
			return
		}
	}
}

// reserveDial counts a dial in flight unless the reactor stopped or the
// entry's IP group was dialed recently.
func (r *Reactor) reserveDial(e Entry) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	group, now := ipGroup(e.ip), r.now()
	if r.stopped || now.Sub(r.groupDials[group]) < groupDialInterval {
		return false
	}
	for g, at := range r.groupDials {
		if now.Sub(at) >= groupDialInterval {
			delete(r.groupDials, g)
		}
	}

	r.groupDials[group] = now
	r.dialing++
	r.workers.Add(1)

	return true
}

func (r *Reactor) dial(e Entry) {
	defer r.workers.Done()
	defer func() {
		r.mu.Lock()
		r.dialing--
		r.mu.Unlock()
	}()

	err := r.dialer.DialPeer(r.ctx, peer.AddrInfo{ID: e.ID, Addrs: []ma.Multiaddr{e.Addr}})
	r.recordDial(e, err)
}

// recordDial counts success only if we are connected at the dialed address. A
// peer ID mismatch only drops the address, since anyone can gossip an honest
// ID with their own IP.
func (r *Reactor) recordDial(e Entry, err error) {
	var mismatch sec.ErrPeerIDMismatch
	cause := dialCause(err, e.Addr)

	switch {
	case err == nil && !r.connectedAt(e.ID, e.Addr):
		r.Logger.Debug("Dialed peer is connected at another address", "peer_id", e.ID, "addr", e.Addr)
	case err == nil:
		r.Logger.Debug("Dialed peer", "peer_id", e.ID, "addr", e.Addr)
		if err := r.book.AddVerified(e.ID, e.Addr, SourceDialed); err != nil {
			r.Logger.Debug("Dropping dialed address", "peer_id", e.ID, "addr", e.Addr, "err", err)
		}
		r.book.MarkDialed(e.ID)
	case errors.As(cause, &mismatch):
		r.Logger.Debug("Dropping address of another peer", "peer_id", e.ID, "addr", e.Addr)
		r.book.RemoveAddr(e.ID, e.Addr)
	case errors.Is(cause, lp2p.ErrSwitchInactive), errors.Is(cause, context.Canceled):
		r.Logger.Debug("Dial interrupted", "peer_id", e.ID, "err", err)
	case errors.Is(cause, lp2p.ErrDialBusy),
		errors.Is(cause, lp2p.ErrPeerConnected),
		errors.Is(cause, swarm.ErrGaterDisallowedConnection),
		errors.Is(cause, swarm.ErrDialBackoff),
		errors.Is(cause, network.ErrResourceLimitExceeded):
		r.Logger.Debug("Peer busy or dial limited", "peer_id", e.ID, "err", err)
		r.book.SetCooldown(e.ID, r.now().Add(busyCooldown))
	default:
		r.Logger.Debug("Failed to dial peer", "peer_id", e.ID, "addr", e.Addr, "err", err)
		r.book.MarkAttempt(e.ID)
	}
}

// dialCause narrows a swarm dial error to what happened at addr, since it
// also carries errors from the peer's other known addresses. A nil err gives nil.
func dialCause(err error, addr ma.Multiaddr) error {
	var dialErr *swarm.DialError
	if !errors.As(err, &dialErr) {
		return err
	}

	for _, te := range dialErr.DialErrors {
		if te.Address.Equal(addr) {
			return te.Cause
		}
	}
	return dialErr.Cause
}

func (r *Reactor) connectedAt(id peer.ID, addr ma.Multiaddr) bool {
	for _, c := range r.host.Network().ConnsToPeer(id) {
		if c.RemoteMultiaddr().Equal(addr) {
			return true
		}
	}

	return false
}

func jitter(d time.Duration) time.Duration {
	return d + rand.N(d/10+1)
}

func (r *Reactor) expire(peers []p2p.Peer) {
	connected := make(map[p2p.ID]struct{}, len(peers))
	for _, p := range peers {
		connected[p.ID()] = struct{}{}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	for id, st := range r.peers {
		_, ok := connected[id]
		switch {
		case !ok && st.requested.IsZero():
			delete(r.peers, id)
		case !st.requested.IsZero() && now.Sub(st.requested) > requestTimeout:
			st.requested, st.timedOut = time.Time{}, true
		}
	}
}

func (r *Reactor) timedOut(id p2p.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	st, ok := r.peers[id]
	return ok && st.timedOut
}

func (r *Reactor) hasRequest(id p2p.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	st, ok := r.peers[id]
	return ok && !st.requested.IsZero()
}

// askable reports whether the peer speaks PEX and is neither banned nor
// private; a private peer's ID must not end up in the book as an origin.
func (r *Reactor) askable(p p2p.Peer) bool {
	id, err := peerID(p)
	if err != nil || r.book.IsBanned(id) || contains(r.book.cfg.Private, id) {
		return false
	}

	protocols, err := r.host.Peerstore().SupportsProtocols(id, lp2p.ProtocolID(Channel))

	return err == nil && len(protocols) > 0
}

// verifyInboundAddrs stores the listen address of an inbound PEX peer once
// identify shows it dialed us from that address.
func (r *Reactor) verifyInboundAddrs() {
	defer r.workers.Done()

	for evt := range r.identifySub.Out() {
		e, ok := evt.(event.EvtPeerIdentificationCompleted)
		if !ok || e.Conn == nil || e.Conn.Stat().Direction != network.DirInbound ||
			!slices.Contains(e.Protocols, lp2p.ProtocolID(Channel)) {
			continue
		}

		addr, ok := VerifiedInboundAddr(e.Conn.RemoteMultiaddr(), e.ListenAddrs)
		if !ok {
			continue
		}

		if err := r.book.AddVerified(e.Peer, addr, SourceInbound); err != nil {
			r.Logger.Debug("Dropping verified inbound address", "peer_id", e.Peer, "addr", addr, "err", err)
		}
	}
}

func peerID(p p2p.Peer) (peer.ID, error) {
	return peer.Decode(string(p.ID()))
}

func parseNetAddress(na tmp2p.NetAddress) (peer.ID, ma.Multiaddr, error) {
	if len(na.ID) > maxIDLen {
		return "", nil, fmt.Errorf("peer ID too long: %d bytes", len(na.ID))
	}

	id, err := peer.Decode(na.ID)
	if err != nil {
		return "", nil, errors.New("undecodable peer ID")
	}

	ip := net.ParseIP(na.IP)
	if ip == nil {
		return "", nil, errors.New("invalid IP")
	}

	if na.Port == 0 || na.Port > 65535 {
		return "", nil, fmt.Errorf("port %d", na.Port)
	}

	proto := "ip6"
	if ip4 := ip.To4(); ip4 != nil {
		proto, ip = "ip4", ip4
	}

	addr, err := ma.NewMultiaddr(fmt.Sprintf("/%s/%s/udp/%d/quic-v1", proto, ip, na.Port))
	if err != nil {
		return "", nil, err
	}

	return id, addr, nil
}
