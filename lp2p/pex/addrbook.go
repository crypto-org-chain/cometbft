// Package pex provides the address book and reactor for libp2p peer exchange.
package pex

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"net"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/cometbft/cometbft/libs/tempfile"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

const bookVersion = 2

// maxIDBytes bounds raw peer IDs; real ones are at most 39 bytes.
const maxIDBytes = 64

const (
	retriesBeforeBad  = 3
	failuresBeforeBad = 10
	recentAttempt     = time.Minute
)

// Stored times stay within the range JSON can encode.
var (
	minTime = time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)
	maxTime = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
)

// Source tells how the book learned an address.
type Source string

const (
	SourceGossip  Source = "gossip"
	SourceInbound Source = "inbound"
	SourceDialed  Source = "dialed"
)

var (
	ErrInvalidID      = errors.New("invalid peer ID")
	ErrSelf           = errors.New("own peer ID")
	ErrPrivate        = errors.New("private peer")
	ErrBootstrap      = errors.New("bootstrap peer")
	ErrBanned         = errors.New("banned peer")
	ErrInvalidAddr    = errors.New("invalid address")
	ErrUnroutable     = errors.New("unroutable address")
	ErrInvalidSource  = errors.New("invalid source or origin")
	ErrOriginLimit    = errors.New("too many addresses from origin")
	ErrGroupLimit     = errors.New("too many addresses in IP group")
	ErrDiversityLimit = errors.New("too many addresses in diversity group")
	ErrBookFull       = errors.New("address book is full")
)

// Config sets the address book limits. Private peers are never stored, banned
// or accepted as gossip origins; bootstrap peers are never stored or banned.
// Start from DefaultConfig: zero limits are not treated as unlimited.
type Config struct {
	Self      peer.ID
	Private   map[peer.ID]struct{}
	Bootstrap map[peer.ID]struct{}

	// Strict rejects non-public addresses and caps diversity groups.
	Strict           bool
	AllowDuplicateIP bool

	MaxSize              int
	MaxPerOrigin         int
	MaxPerGroup          int
	MaxPerDiversityGroup int
	MaxBans              int

	// StaleAfter is how long a peer may go unseen, or without a success,
	// before it counts as bad or stale and becomes evictable.
	StaleAfter time.Duration

	// Connected reports peers that must never be evicted. Optional. It runs
	// under the book's lock, so it must not block or call into the book.
	Connected func(peer.ID) bool
}

// DefaultConfig returns the default limits.
func DefaultConfig() Config {
	return Config{
		Private:              map[peer.ID]struct{}{},
		Bootstrap:            map[peer.ID]struct{}{},
		Strict:               true,
		MaxSize:              1000,
		MaxPerOrigin:         128,
		MaxPerGroup:          2,
		MaxPerDiversityGroup: 32,
		MaxBans:              1000,
		StaleAfter:           7 * 24 * time.Hour,
	}
}

// Entry is one known peer.
type Entry struct {
	ID            peer.ID      `json:"id"`
	Addr          ma.Multiaddr `json:"addr"`
	Source        Source       `json:"source"`
	Origin        peer.ID      `json:"origin,omitempty"`
	LastSeen      time.Time    `json:"last_seen"`
	Attempts      int          `json:"attempts"`
	LastAttempt   time.Time    `json:"last_attempt"`
	LastDialedOK  time.Time    `json:"last_dialed_ok"`
	LastGood      time.Time    `json:"last_good"`
	CooldownUntil time.Time    `json:"cooldown_until"`

	ip net.IP
}

type bookFile struct {
	Version int         `json:"version"`
	Entries []Entry     `json:"entries"`
	Bans    []banRecord `json:"bans"`
}

type banRecord struct {
	ID    peer.ID   `json:"id"`
	Until time.Time `json:"until"`
}

// AddrBook stores peers learned through PEX. It is safe for concurrent use.
type AddrBook struct {
	path string
	cfg  Config
	now  func() time.Time

	saveMu sync.Mutex

	mu      sync.Mutex
	entries map[peer.ID]*Entry
	bans    map[peer.ID]time.Time
}

// NewAddrBook creates an empty book persisted at path.
func NewAddrBook(path string, cfg Config) *AddrBook {
	cfg.Private = maps.Clone(cfg.Private)
	cfg.Bootstrap = maps.Clone(cfg.Bootstrap)

	return &AddrBook{
		path:    path,
		cfg:     cfg,
		now:     time.Now,
		entries: make(map[peer.ID]*Entry),
		bans:    make(map[peer.ID]time.Time),
	}
}

// AddGossiped stores an address received from origin. Gossip never replaces a known address.
func (b *AddrBook) AddGossiped(id peer.ID, addr ma.Multiaddr, origin peer.ID) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !validID(origin) {
		return ErrInvalidSource
	}
	if contains(b.cfg.Private, origin) {
		return ErrPrivate
	}
	if b.isBanned(origin) {
		return ErrBanned
	}

	ip, err := b.admit(id, addr)
	if err != nil {
		return err
	}

	if _, ok := b.entries[id]; ok {
		return nil
	}

	if b.countOrigin(origin) >= b.cfg.MaxPerOrigin {
		return ErrOriginLimit
	}

	if err := b.makeRoom(id, ip, reclaimBad); err != nil {
		return err
	}

	b.entries[id] = &Entry{ID: id, Addr: addr, Source: SourceGossip, Origin: origin, LastSeen: b.now(), ip: ip}

	return nil
}

// AddVerified stores an address confirmed by a direct connection and marks the
// peer as just seen. It replaces any known address for the peer and may
// displace bad entries or unproven gossip to fit its IP groups. It is not a
// success: call MarkDialed or MarkGood for that.
func (b *AddrBook) AddVerified(id peer.ID, addr ma.Multiaddr, source Source) error {
	if source != SourceInbound && source != SourceDialed {
		return ErrInvalidSource
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	ip, err := b.admit(id, addr)
	if err != nil {
		return err
	}

	if e, ok := b.entries[id]; ok {
		if err := b.reserveGroups(id, ip, reclaimUnprovenGossip); err != nil {
			return err
		}
		e.Addr, e.ip, e.Source, e.Origin = addr, ip, source, ""
		e.Attempts, e.LastSeen = 0, b.now()
		return nil
	}

	if err := b.makeRoom(id, ip, reclaimUnprovenGossip); err != nil {
		return err
	}

	b.entries[id] = &Entry{ID: id, Addr: addr, Source: source, LastSeen: b.now(), ip: ip}

	return nil
}

// Get returns a copy of the entry for id.
func (b *AddrBook) Get(id peer.ID) (Entry, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	e, ok := b.entries[id]
	if !ok {
		return Entry{}, false
	}

	return *e, true
}

// Gossipable returns up to n random entries that are safe to share: verified
// addresses and gossip we dialed that are not bad, never the excluded peer.
func (b *AddrBook) Gossipable(exclude peer.ID, n int) []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	candidates := make([]*Entry, 0, len(b.entries))
	for id, e := range b.entries {
		if rank, _ := b.rank(e, now); id != exclude && rank != rankBad && rank != rankUnprovenGossip {
			candidates = append(candidates, e)
		}
	}

	n = max(0, min(n, len(candidates)))
	entries := make([]Entry, n)
	for i := range n {
		j := i + rand.IntN(len(candidates)-i)
		candidates[i], candidates[j] = candidates[j], candidates[i]
		entries[i] = *candidates[i]
	}

	return entries
}

// Size returns the number of stored peers.
func (b *AddrBook) Size() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.entries)
}

// NeedMoreAddrs reports whether the book is below half its capacity.
func (b *AddrBook) NeedMoreAddrs() bool {
	return b.Size() < b.cfg.MaxSize/2
}

// MarkAttempt records a failed dial.
func (b *AddrBook) MarkAttempt(id peer.ID) {
	b.update(id, func(e *Entry) {
		e.Attempts++
		e.LastAttempt = b.now()
	})
}

// MarkDialed records a successful dial.
func (b *AddrBook) MarkDialed(id peer.ID) {
	b.update(id, func(e *Entry) {
		e.Attempts = 0
		e.LastAttempt = b.now()
		e.LastDialedOK = e.LastAttempt
	})
}

// MarkGood records that the peer helped consensus.
func (b *AddrBook) MarkGood(id peer.ID) {
	b.update(id, func(e *Entry) { e.LastGood = b.now() })
}

// SetCooldown keeps a peer from being dialed until the given time.
func (b *AddrBook) SetCooldown(id peer.ID, until time.Time) {
	b.update(id, func(e *Entry) { e.CooldownUntil = clampTime(until) })
}

// InCooldown reports whether the peer must not be dialed yet.
func (b *AddrBook) InCooldown(id peer.ID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	e, ok := b.entries[id]
	return ok && b.now().Before(e.CooldownUntil)
}

// Ban removes the peer and, while MaxBans allows, refuses it until the given
// time. A ban is only ever extended. Bootstrap and private peers are never banned.
func (b *AddrBook) Ban(id peer.ID, until time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.entries, id)
	b.addBan(id, until)
}

// IsBanned reports whether the peer is banned.
func (b *AddrBook) IsBanned(id peer.ID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.isBanned(id)
}

// Save writes the book to disk atomically.
func (b *AddrBook) Save() error {
	b.saveMu.Lock()
	defer b.saveMu.Unlock()

	b.mu.Lock()
	b.pruneBans()
	file := bookFile{Version: bookVersion, Entries: make([]Entry, 0, len(b.entries)), Bans: make([]banRecord, 0, len(b.bans))}
	for _, e := range b.entries {
		file.Entries = append(file.Entries, *e)
	}
	for id, until := range b.bans {
		file.Bans = append(file.Bans, banRecord{ID: id, Until: until})
	}
	b.mu.Unlock()

	bz, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("marshal address book: %w", err)
	}

	return tempfile.WriteFileAtomic(b.path, bz, 0o600)
}

// Load reads the book from disk. A missing file leaves the book empty. Every
// entry goes through the same checks and caps as an add, best entries first.
func (b *AddrBook) Load() error {
	bz, err := os.ReadFile(b.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read address book: %w", err)
	}

	var file bookFile
	if err := json.Unmarshal(bz, &file); err != nil {
		return fmt.Errorf("decode address book: %w", err)
	}
	if file.Version != bookVersion {
		return fmt.Errorf("address book version %d, want %d", file.Version, bookVersion)
	}

	slices.SortFunc(file.Entries, func(x, y Entry) int {
		if c := lastSuccess(&y).Compare(lastSuccess(&x)); c != 0 {
			return c
		}
		if vx, vy := x.Source != SourceGossip, y.Source != SourceGossip; vx != vy {
			if vx {
				return -1
			}
			return 1
		}
		return y.LastSeen.Compare(x.LastSeen)
	})

	b.mu.Lock()
	defer b.mu.Unlock()

	for _, ban := range file.Bans {
		b.addBan(ban.ID, ban.Until)
	}

	for i := range file.Entries {
		b.load(&file.Entries[i])
	}

	return nil
}

func (b *AddrBook) load(e *Entry) {
	if len(b.entries) >= b.cfg.MaxSize || e.Addr == nil {
		return
	}
	if _, ok := b.entries[e.ID]; ok {
		return
	}

	switch e.Source {
	case SourceGossip:
		if !validID(e.Origin) || contains(b.cfg.Private, e.Origin) || b.countOrigin(e.Origin) >= b.cfg.MaxPerOrigin {
			return
		}
	case SourceInbound, SourceDialed:
	default:
		return
	}

	ip, err := b.admit(e.ID, e.Addr)
	if err != nil || b.reserveGroups(e.ID, ip, reclaimNone) != nil {
		return
	}

	cp := *e
	cp.ip = ip
	b.entries[e.ID] = &cp
}

// VerifiedInboundAddr returns the advertised listen address that matches the
// connection's remote IP and port. It only matches when the remote dials from
// its listen socket, which QUIC does when it reuses that socket.
func VerifiedInboundAddr(remote ma.Multiaddr, listen []ma.Multiaddr) (ma.Multiaddr, bool) {
	rip, rport, err := ipPort(remote)
	if err != nil {
		return nil, false
	}

	for _, addr := range listen {
		ip, port, err := ipPort(addr)
		if err == nil && port == rport && ip.Equal(rip) {
			return addr, true
		}
	}

	return nil, false
}

func (b *AddrBook) admit(id peer.ID, addr ma.Multiaddr) (net.IP, error) {
	switch {
	case !validID(id):
		return nil, ErrInvalidID
	case id == b.cfg.Self:
		return nil, ErrSelf
	case contains(b.cfg.Private, id):
		return nil, ErrPrivate
	case contains(b.cfg.Bootstrap, id):
		return nil, ErrBootstrap
	case b.isBanned(id):
		return nil, ErrBanned
	}

	ip, _, err := ipPort(addr)
	if err != nil {
		return nil, err
	}
	if ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.IPv4bcast) {
		return nil, ErrUnroutable
	}
	if b.cfg.Strict && !manet.IsPublicAddr(addr) {
		return nil, ErrUnroutable
	}

	return ip, nil
}

func (b *AddrBook) makeRoom(id peer.ID, ip net.IP, policy int) error {
	if err := b.reserveGroups(id, ip, policy); err != nil {
		return err
	}
	if len(b.entries) < b.cfg.MaxSize {
		return nil
	}

	victim, ok := b.evictionCandidate()
	if !ok {
		return ErrBookFull
	}
	delete(b.entries, victim)

	return nil
}

// reserveGroups enforces the IP group and diversity caps. A full group frees
// the slot of its oldest reclaimable entry; nothing is removed unless both caps pass.
func (b *AddrBook) reserveGroups(id peer.ID, ip net.IP, policy int) error {
	limits := []struct {
		enabled bool
		key     func(net.IP) string
		max     int
		err     error
	}{
		{enabled: !b.cfg.AllowDuplicateIP, key: ipGroup, max: b.cfg.MaxPerGroup, err: ErrGroupLimit},
		{enabled: b.cfg.Strict, key: diversityGroup, max: b.cfg.MaxPerDiversityGroup, err: ErrDiversityLimit},
	}

	now := b.now()
	freed := make(map[peer.ID]struct{})

	for _, limit := range limits {
		if !limit.enabled {
			continue
		}

		var (
			key    = limit.key(ip)
			count  int
			victim *Entry
		)
		for otherID, e := range b.entries {
			if _, ok := freed[otherID]; ok || otherID == id || limit.key(e.ip) != key {
				continue
			}
			count++
			if (victim == nil || before(0, e.LastSeen, e.ID, 0, victim.LastSeen, victim.ID)) && b.reclaimable(e, now, policy) {
				victim = e
			}
		}

		if count < limit.max {
			continue
		}
		if victim == nil {
			return limit.err
		}
		freed[victim.ID] = struct{}{}
	}

	for victim := range freed {
		delete(b.entries, victim)
	}

	return nil
}

// Entry ranks, from first to last evicted.
const (
	rankBad = iota
	rankUnprovenGossip
	rankUnprovenVerified
	rankStale
	rankKeep
)

// Group reclaim policies: the highest rank that may give up its group slot.
const (
	reclaimNone           = -1
	reclaimBad            = rankBad
	reclaimUnprovenGossip = rankUnprovenGossip
)

// rank orders entries for eviction: bad, unproven gossip, verified entries
// that never succeeded, then stale successes. It also returns the time that
// orders entries within the rank, oldest first.
func (b *AddrBook) rank(e *Entry, now time.Time) (int, time.Time) {
	success := lastSuccess(e)

	switch {
	case b.isBad(e, now):
		return rankBad, latest(e.LastSeen, e.LastAttempt)
	case isUnprovenGossip(e):
		return rankUnprovenGossip, e.LastSeen
	case success.IsZero():
		return rankUnprovenVerified, e.LastSeen
	case now.Sub(success) > b.cfg.StaleAfter:
		return rankStale, success
	default:
		return rankKeep, time.Time{}
	}
}

// reclaimable reports whether e may give up its group slot under the policy.
// Connected entries never do.
func (b *AddrBook) reclaimable(e *Entry, now time.Time, policy int) bool {
	rank, _ := b.rank(e, now)
	return rank <= policy && !b.isConnected(e.ID)
}

// evictionCandidate picks the lowest-ranked unconnected entry, oldest first.
// Like the classic book, it does not depend on the newcomer's source.
func (b *AddrBook) evictionCandidate() (peer.ID, bool) {
	now := b.now()

	var (
		victim   peer.ID
		bestRank = rankKeep
		bestAt   time.Time
	)
	for id, e := range b.entries {
		rank, at := b.rank(e, now)
		if rank == rankKeep || !before(rank, at, id, bestRank, bestAt, victim) || b.isConnected(id) {
			continue
		}
		victim, bestRank, bestAt = id, rank, at
	}

	return victim, bestRank < rankKeep
}

// before orders candidates by rank, then time, then peer ID for determinism.
func before(rank int, at time.Time, id peer.ID, otherRank int, otherAt time.Time, otherID peer.ID) bool {
	if rank != otherRank {
		return rank < otherRank
	}
	if !at.Equal(otherAt) {
		return at.Before(otherAt)
	}

	return id < otherID
}

// isBad ports the classic rule. An entry that was tried in the last minute or
// was good within StaleAfter is never bad. Otherwise it is bad when it went
// unseen for StaleAfter, never succeeded after a few tries, or kept failing
// since its last success went stale.
func (b *AddrBook) isBad(e *Entry, now time.Time) bool {
	if now.Sub(e.LastAttempt) < recentAttempt {
		return false
	}
	if !e.LastGood.IsZero() && now.Sub(e.LastGood) <= b.cfg.StaleAfter {
		return false
	}

	switch {
	case now.Sub(latest(e.LastSeen, e.LastAttempt)) > b.cfg.StaleAfter:
		return true
	case lastSuccess(e).IsZero() && e.Attempts >= retriesBeforeBad:
		return true
	case now.Sub(lastSuccess(e)) > b.cfg.StaleAfter && e.Attempts >= failuresBeforeBad:
		return true
	}

	return false
}

func (b *AddrBook) isConnected(id peer.ID) bool {
	return b.cfg.Connected != nil && b.cfg.Connected(id)
}

func (b *AddrBook) update(id peer.ID, fn func(*Entry)) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if e, ok := b.entries[id]; ok {
		fn(e)
	}
}

func (b *AddrBook) countOrigin(origin peer.ID) int {
	n := 0
	for _, e := range b.entries {
		if e.Origin == origin {
			n++
		}
	}

	return n
}

func (b *AddrBook) isBanned(id peer.ID) bool {
	until, ok := b.bans[id]
	return ok && b.now().Before(until)
}

// addBan keeps the longest ban per peer. At MaxBans the earliest-expiring ban
// gives way, unless the new one would expire first.
func (b *AddrBook) addBan(id peer.ID, until time.Time) {
	if !validID(id) || contains(b.cfg.Bootstrap, id) || contains(b.cfg.Private, id) || !until.After(b.now()) {
		return
	}
	until = clampTime(until)

	if current, ok := b.bans[id]; ok {
		b.bans[id] = latest(current, until)
		return
	}

	if len(b.bans) >= b.cfg.MaxBans {
		var (
			soonest peer.ID
			at      time.Time
		)
		for other, otherUntil := range b.bans {
			if at.IsZero() || otherUntil.Before(at) {
				soonest, at = other, otherUntil
			}
		}
		if at.IsZero() || !until.After(at) {
			return
		}
		delete(b.bans, soonest)
	}

	b.bans[id] = until
}

func (b *AddrBook) pruneBans() {
	now := b.now()
	for id, until := range b.bans {
		if !now.Before(until) {
			delete(b.bans, id)
		}
	}
}

// ipPort accepts exactly /ip4|ip6/<ip>/udp/<port>/quic-v1.
func ipPort(addr ma.Multiaddr) (net.IP, int, error) {
	if len(addr) != 3 ||
		(addr[0].Code() != ma.P_IP4 && addr[0].Code() != ma.P_IP6) ||
		addr[1].Code() != ma.P_UDP ||
		addr[2].Code() != ma.P_QUIC_V1 {
		return nil, 0, fmt.Errorf("%w: %s", ErrInvalidAddr, addr)
	}

	port, err := strconv.Atoi(addr[1].Value())
	if err != nil || port == 0 {
		return nil, 0, fmt.Errorf("%w: %s", ErrInvalidAddr, addr)
	}

	return net.IP(addr[0].RawValue()), port, nil
}

// ipGroup keys per-IP limits: IPv4 /32 and IPv6 /64.
func ipGroup(ip net.IP) string {
	if ip4 := ip.To4(); ip4 != nil {
		return ip4.String()
	}

	return ip.Mask(net.CIDRMask(64, 128)).String()
}

// diversityGroup keys diversity limits: IPv4 /16 and IPv6 /32.
func diversityGroup(ip net.IP) string {
	if ip4 := ip.To4(); ip4 != nil {
		return ip4.Mask(net.CIDRMask(16, 32)).String()
	}

	return ip.Mask(net.CIDRMask(32, 128)).String()
}

func isUnprovenGossip(e *Entry) bool {
	return e.Source == SourceGossip && lastSuccess(e).IsZero()
}

func lastSuccess(e *Entry) time.Time {
	return latest(e.LastDialedOK, e.LastGood)
}

func clampTime(t time.Time) time.Time {
	switch {
	case t.Before(minTime):
		return minTime
	case t.After(maxTime):
		return maxTime
	default:
		return t.UTC()
	}
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}

	return b
}

// validID accepts only short IDs that survive a text round trip, so Save output
// stays loadable and decoding stays cheap.
func validID(id peer.ID) bool {
	if len(id) > maxIDBytes {
		return false
	}

	decoded, err := peer.Decode(id.String())
	return err == nil && decoded == id
}

func contains(set map[peer.ID]struct{}, id peer.ID) bool {
	_, ok := set[id]
	return ok
}
