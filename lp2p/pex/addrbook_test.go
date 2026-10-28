package pex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	p2ptest "github.com/libp2p/go-libp2p/core/test"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/stretchr/testify/suite"
)

type addrBookSuite struct {
	suite.Suite

	clock time.Time
}

func TestAddrBookSuite(t *testing.T) {
	suite.Run(t, new(addrBookSuite))
}

func (s *addrBookSuite) SetupTest() {
	s.clock = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
}

func (s *addrBookSuite) newBook(malleate func(*Config)) *AddrBook {
	cfg := DefaultConfig()
	if malleate != nil {
		malleate(&cfg)
	}

	b := NewAddrBook(filepath.Join(s.T().TempDir(), "addrbook.json"), cfg)
	b.now = func() time.Time { return s.clock }

	return b
}

func (s *addrBookSuite) id() peer.ID {
	return p2ptest.RandPeerIDFatal(s.T())
}

func (s *addrBookSuite) markAttempts(b *AddrBook, id peer.ID, n int) {
	for range n {
		b.MarkAttempt(id)
	}
}

func (s *addrBookSuite) dialedAgo(b *AddrBook, id peer.ID, ago time.Duration) {
	now := s.clock
	s.clock = now.Add(-ago)
	b.MarkDialed(id)
	s.clock = now
}

// sha1PeerID is a well-formed multihash that peer IDs cannot encode as text.
func sha1PeerID() peer.ID {
	return peer.ID(append([]byte{0x11, 20}, make([]byte, 20)...))
}

// identityPeerID is a valid identity peer ID of 2+digest raw bytes.
func identityPeerID(digest int) peer.ID {
	return peer.ID(append([]byte{0x00, byte(digest)}, make([]byte, digest)...))
}

// hugePeerID is a valid identity peer ID far longer than any real one.
func hugePeerID() peer.ID {
	return peer.ID(append([]byte{0x00, 0xc8, 0x01}, make([]byte, 200)...))
}

func addr(ip string, port int) ma.Multiaddr {
	proto := "ip4"
	if strings.Contains(ip, ":") {
		proto = "ip6"
	}

	return ma.StringCast(fmt.Sprintf("/%s/%s/udp/%d/quic-v1", proto, ip, port))
}

func (s *addrBookSuite) TestAdmission() {
	self, private, bootstrap, banned := s.id(), s.id(), s.id(), s.id()

	testCases := []struct {
		name     string
		id       peer.ID
		addr     ma.Multiaddr
		strict   bool
		expected error
	}{
		{name: "public address", id: s.id(), addr: addr("8.8.8.8", 26656), strict: true},
		{name: "invalid peer ID", id: peer.ID("abc"), addr: addr("8.8.8.8", 26656), strict: true, expected: ErrInvalidID},
		{name: "empty peer ID", id: "", addr: addr("8.8.8.8", 26656), strict: true, expected: ErrInvalidID},
		{name: "peer ID without a text form", id: sha1PeerID(), addr: addr("8.8.8.8", 26656), strict: true, expected: ErrInvalidID},
		{name: "oversized peer ID", id: hugePeerID(), addr: addr("8.8.8.8", 26656), strict: true, expected: ErrInvalidID},
		{name: "peer ID at the size limit", id: identityPeerID(maxIDBytes - 2), addr: addr("8.8.8.8", 26656), strict: true},
		{name: "peer ID over the size limit", id: identityPeerID(maxIDBytes - 1), addr: addr("8.8.8.8", 26656), strict: true, expected: ErrInvalidID},
		{name: "self", id: self, addr: addr("8.8.8.8", 26656), strict: true, expected: ErrSelf},
		{name: "private peer", id: private, addr: addr("8.8.8.8", 26656), strict: true, expected: ErrPrivate},
		{name: "bootstrap peer", id: bootstrap, addr: addr("8.8.8.8", 26656), strict: true, expected: ErrBootstrap},
		{name: "banned peer", id: banned, addr: addr("8.8.8.8", 26656), strict: true, expected: ErrBanned},
		{name: "port zero", id: s.id(), addr: addr("8.8.8.8", 0), strict: true, expected: ErrInvalidAddr},
		{name: "tcp address", id: s.id(), addr: ma.StringCast("/ip4/8.8.8.8/tcp/26656"), expected: ErrInvalidAddr},
		{name: "dns address", id: s.id(), addr: ma.StringCast("/dns4/example.com/udp/26656/quic-v1"), expected: ErrInvalidAddr},
		{name: "udp without quic", id: s.id(), addr: ma.StringCast("/ip4/8.8.8.8/udp/26656"), expected: ErrInvalidAddr},
		{name: "extra leading component", id: s.id(), addr: ma.StringCast("/ip4/8.8.8.8/tcp/5/udp/1/quic-v1"), expected: ErrInvalidAddr},
		{name: "repeated quic", id: s.id(), addr: ma.StringCast("/ip4/8.8.8.8/udp/1/quic-v1/udp/2/quic-v1"), expected: ErrInvalidAddr},
		{name: "webtransport suffix", id: s.id(), addr: ma.StringCast("/ip4/8.8.8.8/udp/1/quic-v1/webtransport"), expected: ErrInvalidAddr},
		{name: "relay address", id: s.id(), addr: ma.StringCast("/ip4/8.8.8.8/udp/1/quic-v1/p2p/" + s.id().String() + "/p2p-circuit"), expected: ErrInvalidAddr},
		{name: "unspecified address", id: s.id(), addr: addr("0.0.0.0", 26656), expected: ErrUnroutable},
		{name: "unspecified IPv6 address", id: s.id(), addr: addr("::", 26656), expected: ErrUnroutable},
		{name: "multicast address", id: s.id(), addr: addr("224.0.0.1", 26656), expected: ErrUnroutable},
		{name: "broadcast address", id: s.id(), addr: addr("255.255.255.255", 26656), expected: ErrUnroutable},
		{name: "loopback when strict", id: s.id(), addr: addr("127.0.0.1", 26656), strict: true, expected: ErrUnroutable},
		{name: "private IPv6 when strict", id: s.id(), addr: addr("fc00::1", 26656), strict: true, expected: ErrUnroutable},
		{name: "loopback when not strict", id: s.id(), addr: addr("127.0.0.1", 26656)},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			b := s.newBook(func(cfg *Config) {
				cfg.Self = self
				cfg.Private = map[peer.ID]struct{}{private: {}}
				cfg.Bootstrap = map[peer.ID]struct{}{bootstrap: {}}
				cfg.Strict = tc.strict
			})
			b.Ban(banned, s.clock.Add(time.Hour))

			for _, add := range []func() error{
				func() error { return b.AddGossiped(tc.id, tc.addr, s.id()) },
				func() error { return b.AddVerified(tc.id, tc.addr, SourceInbound) },
			} {
				err := add()
				if tc.expected != nil {
					s.Require().ErrorIs(err, tc.expected)
					continue
				}
				s.Require().NoError(err)
			}
		})
	}
}

func (s *addrBookSuite) TestRejectsPrivateOrigin() {
	private := s.id()
	b := s.newBook(func(cfg *Config) { cfg.Private = map[peer.ID]struct{}{private: {}} })

	s.Require().ErrorIs(b.AddGossiped(s.id(), addr("8.8.8.8", 1), private), ErrPrivate)
	s.Zero(b.Size())
}

func (s *addrBookSuite) TestRejectsInvalidSource() {
	b := s.newBook(nil)

	s.Require().ErrorIs(b.AddVerified(s.id(), addr("8.8.8.8", 1), SourceGossip), ErrInvalidSource)
	s.Require().ErrorIs(b.AddGossiped(s.id(), addr("8.8.8.8", 1), ""), ErrInvalidSource)
	s.Require().ErrorIs(b.AddGossiped(s.id(), addr("8.8.8.8", 1), peer.ID("zz")), ErrInvalidSource)
	s.Require().ErrorIs(b.AddGossiped(s.id(), addr("8.8.8.8", 1), sha1PeerID()), ErrInvalidSource)

	banned := s.id()
	b.Ban(banned, s.clock.Add(time.Hour))
	s.Require().ErrorIs(b.AddGossiped(s.id(), addr("8.8.8.8", 1), banned), ErrBanned)
	s.Zero(b.Size())
}

func (s *addrBookSuite) TestConfigMapsAreCopied() {
	private := s.id()
	cfg := DefaultConfig()
	cfg.Private = map[peer.ID]struct{}{}
	b := NewAddrBook(filepath.Join(s.T().TempDir(), "addrbook.json"), cfg)

	cfg.Private[private] = struct{}{}
	cfg.Bootstrap[private] = struct{}{}

	s.Require().NoError(b.AddGossiped(private, addr("8.8.8.8", 1), s.id()))
}

func (s *addrBookSuite) TestAddressReplacement() {
	first, second := addr("8.8.8.8", 1), addr("9.9.9.9", 2)

	testCases := []struct {
		name           string
		add            func(b *AddrBook, id, origin peer.ID)
		expectedAddr   ma.Multiaddr
		expectedSource Source
		expectedOrigin bool
		seenFirst      bool
	}{
		{
			name: "gossip keeps the first gossiped address",
			add: func(b *AddrBook, id, origin peer.ID) {
				s.Require().NoError(b.AddGossiped(id, first, origin))
				s.clock = s.clock.Add(time.Hour)
				s.Require().NoError(b.AddGossiped(id, second, origin))
			},
			expectedAddr:   first,
			expectedSource: SourceGossip,
			expectedOrigin: true,
			seenFirst:      true,
		},
		{
			name: "gossip never replaces a verified address",
			add: func(b *AddrBook, id, origin peer.ID) {
				s.Require().NoError(b.AddVerified(id, first, SourceInbound))
				s.clock = s.clock.Add(time.Hour)
				s.Require().NoError(b.AddGossiped(id, second, origin))
			},
			expectedAddr:   first,
			expectedSource: SourceInbound,
			seenFirst:      true,
		},
		{
			name: "verified address replaces gossip and resets attempts",
			add: func(b *AddrBook, id, origin peer.ID) {
				s.Require().NoError(b.AddGossiped(id, first, origin))
				b.MarkAttempt(id)
				s.Require().NoError(b.AddVerified(id, second, SourceDialed))
			},
			expectedAddr:   second,
			expectedSource: SourceDialed,
		},
		{
			name: "verifying the same address resets attempts and refreshes last seen",
			add: func(b *AddrBook, id, origin peer.ID) {
				s.Require().NoError(b.AddGossiped(id, first, origin))
				b.MarkAttempt(id)
				s.clock = s.clock.Add(time.Hour)
				s.Require().NoError(b.AddVerified(id, first, SourceDialed))
			},
			expectedAddr:   first,
			expectedSource: SourceDialed,
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			b := s.newBook(nil)
			id, origin := s.id(), s.id()
			start := s.clock

			tc.add(b, id, origin)

			e, ok := b.Get(id)
			s.Require().True(ok)
			s.True(tc.expectedAddr.Equal(e.Addr))
			s.Equal(tc.expectedSource, e.Source)
			s.Equal(tc.expectedOrigin, e.Origin == origin)
			s.Zero(e.Attempts)
			if tc.seenFirst {
				s.Equal(start, e.LastSeen)
			} else {
				s.Equal(s.clock, e.LastSeen)
			}
		})
	}
}

func (s *addrBookSuite) TestAddressChangeMovesGroup() {
	b := s.newBook(func(cfg *Config) { cfg.MaxPerGroup = 1 })
	moved := s.id()
	s.Require().NoError(b.AddVerified(moved, addr("8.8.8.8", 1), SourceInbound))
	s.Require().NoError(b.AddVerified(moved, addr("9.9.9.9", 1), SourceInbound))

	s.Require().NoError(b.AddGossiped(s.id(), addr("8.8.8.8", 2), s.id()))
	s.Require().ErrorIs(b.AddGossiped(s.id(), addr("9.9.9.9", 2), s.id()), ErrGroupLimit)
}

func (s *addrBookSuite) TestLimits() {
	testCases := []struct {
		name           string
		malleate       func(*Config)
		existing       []ma.Multiaddr
		sameOrigin     bool
		verifyExisting bool
		next           ma.Multiaddr
		expected       error
	}{
		{
			name:       "origin limit",
			malleate:   func(cfg *Config) { cfg.MaxPerOrigin = 2 },
			existing:   []ma.Multiaddr{addr("1.1.1.1", 1), addr("2.2.2.2", 1)},
			sameOrigin: true,
			next:       addr("3.3.3.3", 1),
			expected:   ErrOriginLimit,
		},
		{
			name:     "other origins are not limited",
			malleate: func(cfg *Config) { cfg.MaxPerOrigin = 2 },
			existing: []ma.Multiaddr{addr("1.1.1.1", 1), addr("2.2.2.2", 1)},
			next:     addr("3.3.3.3", 1),
		},
		{
			name:           "verified entries do not count toward the origin",
			malleate:       func(cfg *Config) { cfg.MaxPerOrigin = 2 },
			existing:       []ma.Multiaddr{addr("1.1.1.1", 1), addr("2.2.2.2", 1)},
			sameOrigin:     true,
			verifyExisting: true,
			next:           addr("3.3.3.3", 1),
		},
		{
			name:     "IPv4 /32 group limit",
			existing: []ma.Multiaddr{addr("1.1.1.1", 1), addr("1.1.1.1", 2)},
			next:     addr("1.1.1.1", 3),
			expected: ErrGroupLimit,
		},
		{
			name:     "IPv6 /64 group limit",
			existing: []ma.Multiaddr{addr("2001:4860:1:1::1", 1), addr("2001:4860:1:1::2", 1)},
			next:     addr("2001:4860:1:1:ffff::3", 1),
			expected: ErrGroupLimit,
		},
		{
			name:     "IPv4 addresses in another /32 are another group",
			existing: []ma.Multiaddr{addr("1.1.1.1", 1), addr("1.1.1.1", 2)},
			next:     addr("1.1.1.2", 1),
		},
		{
			name:     "IPv6 addresses in another /64 are another group",
			existing: []ma.Multiaddr{addr("2001:4860:1:1::1", 1), addr("2001:4860:1:1::2", 1)},
			next:     addr("2001:4860:1:2::1", 1),
		},
		{
			name:     "duplicate IPs allowed",
			malleate: func(cfg *Config) { cfg.AllowDuplicateIP = true },
			existing: []ma.Multiaddr{addr("1.1.1.1", 1), addr("1.1.1.1", 2)},
			next:     addr("1.1.1.1", 3),
		},
		{
			name:     "IPv4 /16 diversity limit",
			malleate: func(cfg *Config) { cfg.MaxPerDiversityGroup = 2 },
			existing: []ma.Multiaddr{addr("1.1.1.1", 1), addr("1.1.2.2", 1)},
			next:     addr("1.1.3.3", 1),
			expected: ErrDiversityLimit,
		},
		{
			name:     "IPv4 addresses in another /16 are another diversity group",
			malleate: func(cfg *Config) { cfg.MaxPerDiversityGroup = 2 },
			existing: []ma.Multiaddr{addr("1.1.1.1", 1), addr("1.1.2.2", 1)},
			next:     addr("1.2.3.3", 1),
		},
		{
			name:     "IPv6 addresses in another /32 are another diversity group",
			malleate: func(cfg *Config) { cfg.MaxPerDiversityGroup = 2 },
			existing: []ma.Multiaddr{addr("2001:4860:1::1", 1), addr("2001:4860:2::1", 1)},
			next:     addr("2001:4861::1", 1),
		},
		{
			name:     "IPv6 /32 diversity limit",
			malleate: func(cfg *Config) { cfg.MaxPerDiversityGroup = 2 },
			existing: []ma.Multiaddr{addr("2001:4860:1::1", 1), addr("2001:4860:2::1", 1)},
			next:     addr("2001:4860:3::1", 1),
			expected: ErrDiversityLimit,
		},
		{
			name: "no diversity limit when not strict",
			malleate: func(cfg *Config) {
				cfg.MaxPerDiversityGroup = 2
				cfg.Strict = false
			},
			existing: []ma.Multiaddr{addr("1.1.1.1", 1), addr("1.1.2.2", 1)},
			next:     addr("1.1.3.3", 1),
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			b := s.newBook(tc.malleate)
			origin := s.id()

			for _, a := range tc.existing {
				id, o := s.id(), origin
				if !tc.sameOrigin {
					o = s.id()
				}
				s.Require().NoError(b.AddGossiped(id, a, o))
				if tc.verifyExisting {
					s.Require().NoError(b.AddVerified(id, a, SourceInbound))
				}
			}

			err := b.AddGossiped(s.id(), tc.next, origin)
			if tc.expected != nil {
				s.Require().ErrorIs(err, tc.expected)
				return
			}
			s.Require().NoError(err)
		})
	}
}

func (s *addrBookSuite) TestGroupReclaim() {
	type holder struct {
		addr     ma.Multiaddr
		verified bool
		prep     func(b *AddrBook, id peer.ID)
	}

	gossip := func(a ma.Multiaddr) holder { return holder{addr: a} }
	verified := func(a ma.Multiaddr) holder { return holder{addr: a, verified: true} }
	dialed := func(a ma.Multiaddr) holder {
		return holder{addr: a, prep: func(b *AddrBook, id peer.ID) { b.MarkDialed(id) }}
	}
	bad := func(h holder) holder {
		h.prep = func(b *AddrBook, id peer.ID) { s.markAttempts(b, id, 3) }
		return h
	}
	addVerified := func(a ma.Multiaddr) func(b *AddrBook, id peer.ID) error {
		return func(b *AddrBook, id peer.ID) error { return b.AddVerified(id, a, SourceInbound) }
	}
	addGossiped := func(a ma.Multiaddr) func(b *AddrBook, id peer.ID) error {
		return func(b *AddrBook, id peer.ID) error { return b.AddGossiped(id, a, s.id()) }
	}

	testCases := []struct {
		name           string
		malleate       func(*Config)
		holders        []holder
		firstConnected bool
		tighten        func(*Config)
		existingAddr   ma.Multiaddr
		moveFirst      bool
		add            func(b *AddrBook, id peer.ID) error
		expected       error
		removed        []int
	}{
		{
			name:    "verified address displaces the least recently seen unproven gossip",
			holders: []holder{gossip(addr("8.8.8.8", 1)), gossip(addr("8.8.8.8", 2))},
			add:     addVerified(addr("8.8.8.8", 3)),
			removed: []int{0},
		},
		{
			name:     "group reclaim makes room in a full book",
			malleate: func(cfg *Config) { cfg.MaxSize = 2 },
			holders:  []holder{gossip(addr("8.8.8.8", 1)), gossip(addr("8.8.8.8", 2))},
			add:      addVerified(addr("8.8.8.8", 3)),
			removed:  []int{0},
		},
		{
			name:     "verified address keeps verified holders",
			holders:  []holder{verified(addr("8.8.8.8", 1)), verified(addr("8.8.8.8", 2))},
			add:      addVerified(addr("8.8.8.8", 3)),
			expected: ErrGroupLimit,
		},
		{
			name:    "verified address displaces bad verified holders",
			holders: []holder{bad(verified(addr("8.8.8.8", 1))), bad(verified(addr("8.8.8.8", 2)))},
			add:     addVerified(addr("8.8.8.8", 3)),
			removed: []int{0},
		},
		{
			name:     "verified address keeps gossip that succeeded",
			holders:  []holder{dialed(addr("8.8.8.8", 1)), dialed(addr("8.8.8.8", 2))},
			add:      addVerified(addr("8.8.8.8", 3)),
			expected: ErrGroupLimit,
		},
		{
			name:           "verified address keeps connected gossip",
			holders:        []holder{gossip(addr("8.8.8.8", 1)), gossip(addr("8.8.8.8", 2))},
			firstConnected: true,
			add:            addVerified(addr("8.8.8.8", 3)),
			removed:        []int{1},
		},
		{
			name:     "gossip keeps unproven gossip",
			holders:  []holder{gossip(addr("8.8.8.8", 1)), gossip(addr("8.8.8.8", 2))},
			add:      addGossiped(addr("8.8.8.8", 3)),
			expected: ErrGroupLimit,
		},
		{
			name:    "gossip displaces bad entries",
			holders: []holder{bad(gossip(addr("8.8.8.8", 1))), bad(gossip(addr("8.8.8.8", 2)))},
			add:     addGossiped(addr("8.8.8.8", 3)),
			removed: []int{0},
		},
		{
			name:           "gossip keeps connected bad entries",
			holders:        []holder{bad(gossip(addr("8.8.8.8", 1))), bad(gossip(addr("8.8.8.8", 2)))},
			firstConnected: true,
			add:            addGossiped(addr("8.8.8.8", 3)),
			removed:        []int{1},
		},
		{
			name:     "verified address displaces unproven gossip in its diversity group",
			malleate: func(cfg *Config) { cfg.MaxPerDiversityGroup = 2 },
			holders:  []holder{gossip(addr("8.8.1.1", 1)), gossip(addr("8.8.2.2", 1))},
			add:      addVerified(addr("8.8.3.3", 1)),
			removed:  []int{0},
		},
		{
			name:         "verified address change displaces unproven gossip",
			holders:      []holder{gossip(addr("8.8.8.8", 1)), gossip(addr("8.8.8.8", 2))},
			existingAddr: addr("9.9.9.9", 1),
			add:          addVerified(addr("8.8.8.8", 3)),
			removed:      []int{0},
		},
		{
			name:         "verified address change into a full group",
			holders:      []holder{verified(addr("8.8.8.8", 1)), verified(addr("8.8.8.8", 2))},
			existingAddr: addr("9.9.9.9", 1),
			add:          addVerified(addr("8.8.8.8", 3)),
			expected:     ErrGroupLimit,
		},
		{
			name:      "verified peer moves within its full group",
			holders:   []holder{verified(addr("8.8.8.8", 1)), verified(addr("8.8.8.8", 2))},
			moveFirst: true,
			add:       addVerified(addr("8.8.8.8", 3)),
		},
		{
			name:     "a failing add removes nothing",
			holders:  []holder{gossip(addr("8.8.8.8", 1)), verified(addr("8.8.8.8", 2)), verified(addr("8.8.9.9", 1))},
			tighten:  func(cfg *Config) { cfg.MaxPerDiversityGroup = 2 },
			add:      addVerified(addr("8.8.8.8", 3)),
			expected: ErrDiversityLimit,
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			for range 10 {
				var connected peer.ID
				b := s.newBook(func(cfg *Config) {
					cfg.Connected = func(id peer.ID) bool { return id == connected }
					if tc.malleate != nil {
						tc.malleate(cfg)
					}
				})

				ids := make([]peer.ID, len(tc.holders))
				for i, h := range tc.holders {
					ids[i] = s.id()
					if h.verified {
						s.Require().NoError(b.AddVerified(ids[i], h.addr, SourceInbound))
					} else {
						s.Require().NoError(b.AddGossiped(ids[i], h.addr, s.id()))
					}
					if h.prep != nil {
						h.prep(b, ids[i])
					}
					s.clock = s.clock.Add(time.Second)
				}
				if tc.firstConnected {
					connected = ids[0]
				}
				if tc.tighten != nil {
					tc.tighten(&b.cfg)
				}
				s.clock = s.clock.Add(2 * recentAttempt)

				id := s.id()
				if tc.moveFirst {
					id = ids[0]
				}
				if tc.existingAddr != nil {
					s.Require().NoError(b.AddVerified(id, tc.existingAddr, SourceInbound))
				}

				err := tc.add(b, id)

				if tc.expected != nil {
					s.Require().ErrorIs(err, tc.expected)
				} else {
					s.Require().NoError(err)
					_, added := b.Get(id)
					s.True(added)
				}
				for i, holderID := range ids {
					_, ok := b.Get(holderID)
					s.Equal(!slices.Contains(tc.removed, i), ok, "holder %d", i)
				}
			}
		})
	}
}

func (s *addrBookSuite) TestIsBad() {
	const stale = 7 * 24 * time.Hour

	testCases := []struct {
		name     string
		entry    Entry
		expected bool
	}{
		{name: "fresh entry", entry: Entry{LastSeen: s.clock}},
		{name: "unseen for a week", entry: Entry{LastSeen: s.clock.Add(-stale - time.Hour)}, expected: true},
		{name: "tried within the week", entry: Entry{LastSeen: s.clock.Add(-stale - time.Hour), LastAttempt: s.clock.Add(-time.Hour), Attempts: 1}},
		{name: "tried in the last minute", entry: Entry{LastSeen: s.clock.Add(-stale - time.Hour), LastAttempt: s.clock.Add(-time.Second), Attempts: 20}},
		{name: "never succeeded after a few tries", entry: Entry{LastSeen: s.clock, LastAttempt: s.clock.Add(-time.Hour), Attempts: 3}, expected: true},
		{name: "few failures since a stale success", entry: Entry{LastSeen: s.clock, LastAttempt: s.clock.Add(-time.Hour), LastDialedOK: s.clock.Add(-stale - time.Hour), Attempts: 9}},
		{name: "kept failing since a stale success", entry: Entry{LastSeen: s.clock, LastAttempt: s.clock.Add(-time.Hour), LastDialedOK: s.clock.Add(-stale - time.Hour), Attempts: 10}, expected: true},
		{name: "kept failing since a recent success", entry: Entry{LastSeen: s.clock, LastAttempt: s.clock.Add(-time.Hour), LastDialedOK: s.clock.Add(-time.Hour), Attempts: 10}},
		{name: "recently good", entry: Entry{LastSeen: s.clock.Add(-stale - time.Hour), LastGood: s.clock.Add(-time.Hour), Attempts: 20}},
		{name: "few failures since a stale good", entry: Entry{LastSeen: s.clock, LastAttempt: s.clock.Add(-time.Hour), LastGood: s.clock.Add(-stale - time.Hour), Attempts: 9}},
		{name: "kept failing since a stale good", entry: Entry{LastSeen: s.clock, LastAttempt: s.clock.Add(-time.Hour), LastGood: s.clock.Add(-stale - time.Hour), Attempts: 10}, expected: true},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			b := s.newBook(nil)
			s.Equal(tc.expected, b.isBad(&tc.entry, s.clock))
		})
	}
}

type evictionSpec struct {
	verified bool
	prep     func(b *AddrBook, id peer.ID)
}

func (s *addrBookSuite) TestEviction() {
	attempts := func(n int) func(b *AddrBook, id peer.ID) {
		return func(b *AddrBook, id peer.ID) { s.markAttempts(b, id, n) }
	}
	dialedAgo := func(ago time.Duration) func(b *AddrBook, id peer.ID) {
		return func(b *AddrBook, id peer.ID) { s.dialedAgo(b, id, ago) }
	}

	var (
		gossip    = evictionSpec{}
		verified  = evictionSpec{verified: true}
		bad       = evictionSpec{prep: attempts(3)}
		stale     = evictionSpec{prep: dialedAgo(8 * 24 * time.Hour)}
		staler    = evictionSpec{prep: dialedAgo(9 * 24 * time.Hour)}
		fresh     = evictionSpec{prep: dialedAgo(time.Hour)}
		goodFails = evictionSpec{prep: func(b *AddrBook, id peer.ID) {
			s.markAttempts(b, id, 10)
			b.MarkGood(id)
		}}
	)

	testCases := []struct {
		name     string
		entries  []evictionSpec
		later    func(b *AddrBook, ids []peer.ID)
		evicted  int
		expected error
	}{
		{name: "bad before unproven gossip", entries: []evictionSpec{gossip, bad}, evicted: 1},
		{name: "unproven gossip before unproven verified", entries: []evictionSpec{verified, gossip}, evicted: 1},
		{name: "unproven verified before stale", entries: []evictionSpec{stale, verified}, evicted: 1},
		{name: "unproven verified is evictable among fresh entries", entries: []evictionSpec{fresh, verified}, evicted: 1},
		{name: "stale before fresh", entries: []evictionSpec{fresh, stale}, evicted: 1},
		{name: "least recently tried bad entry", entries: []evictionSpec{bad, bad}, later: func(b *AddrBook, ids []peer.ID) { b.MarkAttempt(ids[0]) }, evicted: 1},
		{name: "least recently seen unproven gossip", entries: []evictionSpec{gossip, gossip}, evicted: 0},
		{name: "least recently seen unproven verified", entries: []evictionSpec{verified, verified, verified, verified}, evicted: 0},
		{name: "oldest stale success", entries: []evictionSpec{stale, staler}, evicted: 1},
		{name: "full of fresh entries", entries: []evictionSpec{fresh, fresh}, expected: ErrBookFull},
		{name: "recently good entries are kept", entries: []evictionSpec{goodFails, fresh}, expected: ErrBookFull},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			for range 10 {
				s.assertEviction(tc.entries, tc.later, tc.evicted, tc.expected)
			}
		})
	}
}

func (s *addrBookSuite) assertEviction(entries []evictionSpec, later func(*AddrBook, []peer.ID), evicted int, expected error) {
	b := s.newBook(func(cfg *Config) { cfg.MaxSize = len(entries) })

	ids := make([]peer.ID, len(entries))
	for i, e := range entries {
		ids[i] = s.id()
		a := addr(fmt.Sprintf("2.2.%d.2", i), 1)
		s.Require().NoError(b.AddGossiped(ids[i], a, s.id()))
		if e.verified {
			s.Require().NoError(b.AddVerified(ids[i], a, SourceInbound))
		}
		if e.prep != nil {
			e.prep(b, ids[i])
		}
		s.clock = s.clock.Add(time.Minute)
	}
	if later != nil {
		later(b, ids)
	}

	s.clock = s.clock.Add(2 * recentAttempt)
	err := b.AddGossiped(s.id(), addr("3.3.3.3", 1), s.id())

	if expected != nil {
		s.Require().ErrorIs(err, expected)
		s.Equal(len(entries), b.Size())
		return
	}
	s.Require().NoError(err)
	for i, id := range ids {
		_, kept := b.Get(id)
		s.Equal(i != evicted, kept, "entry %d", i)
	}
}

func (s *addrBookSuite) TestEvictionPicksOldestUnconnectedInRank() {
	var connected peer.ID
	b := s.newBook(func(cfg *Config) {
		cfg.MaxSize = 3
		cfg.Connected = func(id peer.ID) bool { return id == connected }
	})

	ids := make([]peer.ID, 3)
	for i := range ids {
		ids[i] = s.id()
		s.Require().NoError(b.AddGossiped(ids[i], addr(fmt.Sprintf("%d.1.1.1", i+1), 1), s.id()))
		s.clock = s.clock.Add(time.Second)
	}
	connected = ids[0]

	s.Require().NoError(b.AddGossiped(s.id(), addr("9.9.9.9", 1), s.id()))

	for i, expected := range []bool{true, false, true} {
		_, kept := b.Get(ids[i])
		s.Equal(expected, kept, "entry %d", i)
	}
}

func (s *addrBookSuite) TestConnectedPeersAreNeverEvicted() {
	connected := s.id()
	b := s.newBook(func(cfg *Config) {
		cfg.MaxSize = 1
		cfg.Connected = func(id peer.ID) bool { return id == connected }
	})
	s.Require().NoError(b.AddGossiped(connected, addr("1.1.1.1", 1), s.id()))

	s.Require().ErrorIs(b.AddGossiped(s.id(), addr("2.2.2.2", 1), s.id()), ErrBookFull)
	s.Require().ErrorIs(b.AddVerified(s.id(), addr("2.2.2.2", 1), SourceInbound), ErrBookFull)
}

func (s *addrBookSuite) TestDialBookkeeping() {
	b := s.newBook(nil)
	id := s.id()
	s.Require().NoError(b.AddGossiped(id, addr("8.8.8.8", 1), s.id()))

	s.markAttempts(b, id, 2)
	e, _ := b.Get(id)
	s.Equal(2, e.Attempts)
	s.Zero(e.LastDialedOK)

	s.clock = s.clock.Add(time.Hour)
	b.MarkDialed(id)
	b.MarkGood(id)
	e, _ = b.Get(id)
	s.Zero(e.Attempts)
	s.Equal(s.clock, e.LastAttempt)
	s.Equal(s.clock, e.LastDialedOK)
	s.Equal(s.clock, e.LastGood)

	b.SetCooldown(id, s.clock.Add(time.Minute))
	s.True(b.InCooldown(id))
	s.clock = s.clock.Add(time.Minute)
	s.False(b.InCooldown(id))
}

func (s *addrBookSuite) TestGossipable() {
	b := s.newBook(nil)
	requester, verified, dialed, unproven, bad := s.id(), s.id(), s.id(), s.id(), s.id()
	s.Require().NoError(b.AddVerified(requester, addr("1.1.1.1", 1), SourceInbound))
	s.Require().NoError(b.AddVerified(verified, addr("2.2.2.2", 1), SourceInbound))
	s.Require().NoError(b.AddGossiped(dialed, addr("3.3.3.3", 1), s.id()))
	b.MarkDialed(dialed)
	s.Require().NoError(b.AddGossiped(unproven, addr("4.4.4.4", 1), s.id()))
	s.Require().NoError(b.AddVerified(bad, addr("5.5.5.5", 1), SourceInbound))
	s.markAttempts(b, bad, 3)
	s.clock = s.clock.Add(2 * recentAttempt)

	testCases := []struct {
		name     string
		n        int
		expected int
	}{
		{name: "all shareable entries", n: 10, expected: 2},
		{name: "capped", n: 1, expected: 1},
		{name: "none", n: 0},
		{name: "negative", n: -1},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			got := b.Gossipable(requester, tc.n)

			s.Len(got, tc.expected)
			for _, e := range got {
				s.Contains([]peer.ID{verified, dialed}, e.ID)
			}
		})
	}
}

func (s *addrBookSuite) TestDialCandidates() {
	type spec struct {
		prep    func(b *AddrBook, id peer.ID)
		proven  bool
		allowed bool
	}
	failedAgo := func(attempts int, ago time.Duration) func(b *AddrBook, id peer.ID) {
		return func(b *AddrBook, id peer.ID) {
			now := s.clock
			s.clock = now.Add(-ago)
			s.markAttempts(b, id, attempts)
			s.clock = now
		}
	}

	testCases := []struct {
		name    string
		entries []spec
	}{
		{name: "fresh entries", entries: []spec{{allowed: true}, {proven: true, allowed: true}}},
		{name: "cooling down", entries: []spec{{prep: func(b *AddrBook, id peer.ID) { b.SetCooldown(id, s.clock.Add(time.Minute)) }}}},
		{name: "inside the dial backoff", entries: []spec{{prep: failedAgo(2, time.Minute)}}},
		{name: "past the dial backoff", entries: []spec{{prep: failedAgo(2, 2*time.Minute), allowed: true}}},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			b := s.newBook(nil)
			var allowed []peer.ID
			for i, e := range tc.entries {
				id := s.id()
				s.Require().NoError(b.AddGossiped(id, addr(fmt.Sprintf("%d.1.1.1", i+1), 1), s.id()))
				if e.proven {
					b.MarkDialed(id)
				}
				if e.prep != nil {
					e.prep(b, id)
				}
				if e.allowed {
					allowed = append(allowed, id)
				}
			}

			s.ElementsMatch(allowed, entryIDs(b.DialCandidates(10)))
		})
	}
}

func (s *addrBookSuite) TestDialCandidatesMix() {
	var connected peer.ID
	b := s.newBook(func(cfg *Config) {
		cfg.Connected = func(id peer.ID) bool { return id == connected }
	})
	proven := make([]peer.ID, 6)
	for i := range proven {
		proven[i] = s.id()
		s.Require().NoError(b.AddGossiped(proven[i], addr(fmt.Sprintf("%d.1.1.1", i+1), 1), s.id()))
		b.MarkDialed(proven[i])
		s.clock = s.clock.Add(time.Second)
	}
	for i := range 6 {
		s.Require().NoError(b.AddGossiped(s.id(), addr(fmt.Sprintf("%d.2.2.2", i+1), 1), s.id()))
	}
	connected = proven[5]

	got := b.DialCandidates(6)

	s.Len(got, 6)
	var provenGot []peer.ID
	for _, e := range got {
		if !e.LastDialedOK.IsZero() {
			provenGot = append(provenGot, e.ID)
		}
	}
	s.Equal([]peer.ID{proven[4], proven[3], proven[2], proven[1]}, provenGot)
	for run := range len(got) / 3 {
		unprovenInRun := 0
		for _, e := range got[3*run : 3*run+3] {
			if e.LastDialedOK.IsZero() {
				unprovenInRun++
			}
		}
		s.Equal(1, unprovenInRun, "run %d", run)
	}
	s.Empty(b.DialCandidates(0))
}

func (s *addrBookSuite) TestDialBackoff() {
	for attempts, expected := range map[int]time.Duration{
		-3: 0, 0: 0, 1: time.Minute, 2: 2 * time.Minute, 7: time.Hour, 20: time.Hour,
	} {
		s.Equal(expected, dialBackoff(attempts), "attempts %d", attempts)
	}
}

func (s *addrBookSuite) TestSizeBySource() {
	b := s.newBook(nil)
	s.Empty(b.SizeBySource())

	upgraded, banned := s.id(), s.id()
	s.Require().NoError(b.AddGossiped(upgraded, addr("8.8.8.8", 1), s.id()))
	s.Require().NoError(b.AddGossiped(banned, addr("8.8.4.4", 1), s.id()))
	s.Require().NoError(b.AddGossiped(s.id(), addr("9.9.9.9", 1), s.id()))
	s.Require().NoError(b.AddVerified(upgraded, addr("8.8.8.8", 1), SourceDialed))
	b.Ban(banned, s.clock.Add(time.Hour))

	s.Require().NoError(b.AddVerified(s.id(), addr("1.1.1.1", 1), SourceInbound))
	sizes := b.SizeBySource()
	s.Equal(map[Source]int{SourceGossip: 1, SourceInbound: 1, SourceDialed: 1}, sizes)
	for source := range sizes {
		s.Contains(allSources[:], source)
	}
}

func (s *addrBookSuite) TestRemoveAddr() {
	b := s.newBook(nil)
	id := s.id()
	s.Require().NoError(b.AddGossiped(id, addr("8.8.8.8", 1), s.id()))

	b.RemoveAddr(id, addr("9.9.9.9", 1))
	_, ok := b.Get(id)
	s.True(ok)

	b.RemoveAddr(id, addr("8.8.8.8", 1))
	_, ok = b.Get(id)
	s.False(ok)
}

func (s *addrBookSuite) TestDialCandidatesSmallNeeds() {
	b := s.newBook(nil)
	best := s.id()
	s.Require().NoError(b.AddGossiped(best, addr("1.1.1.1", 1), s.id()))
	b.MarkDialed(best)
	unproven := make([]peer.ID, 3)
	for i := range unproven {
		unproven[i] = s.id()
		s.Require().NoError(b.AddGossiped(unproven[i], addr(fmt.Sprintf("%d.2.2.2", i+1), 1), s.id()))
	}

	one := b.DialCandidates(1)
	s.Require().Len(one, 1)
	s.Equal(best, one[0].ID)

	s.ElementsMatch(append([]peer.ID{best}, unproven...), entryIDs(b.DialCandidates(4)))
}

func entryIDs(entries []Entry) []peer.ID {
	ids := make([]peer.ID, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return ids
}

func (s *addrBookSuite) TestNeedMoreAddrs() {
	b := s.newBook(func(cfg *Config) { cfg.MaxSize = 4 })
	s.True(b.NeedMoreAddrs())

	for i := range 2 {
		s.Require().NoError(b.AddGossiped(s.id(), addr(fmt.Sprintf("%d.1.1.1", i+1), 1), s.id()))
	}

	s.False(b.NeedMoreAddrs())
}

func (s *addrBookSuite) TestBans() {
	testCases := []struct {
		name     string
		maxBans  int
		bans     []time.Duration
		recorded []bool
		expected []bool
	}{
		{name: "ban", maxBans: 3, bans: []time.Duration{time.Hour}, recorded: []bool{true}, expected: []bool{true}},
		{name: "expired ban", maxBans: 3, bans: []time.Duration{-time.Second}, recorded: []bool{false}, expected: []bool{false}},
		{
			name:     "full: earliest ban gives way",
			maxBans:  2,
			bans:     []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour},
			recorded: []bool{true, true, true},
			expected: []bool{false, true, true},
		},
		{
			name:     "full: shorter new ban is dropped",
			maxBans:  2,
			bans:     []time.Duration{2 * time.Hour, 3 * time.Hour, time.Hour},
			recorded: []bool{true, true, false},
			expected: []bool{true, true, false},
		},
		{name: "no bans allowed", maxBans: 0, bans: []time.Duration{time.Hour}, recorded: []bool{false}, expected: []bool{false}},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			b := s.newBook(func(cfg *Config) { cfg.MaxBans = tc.maxBans })

			ids := make([]peer.ID, len(tc.bans))
			for i, d := range tc.bans {
				ids[i] = s.id()
				s.Equal(tc.recorded[i], b.Ban(ids[i], s.clock.Add(d)), "ban %d", i)
			}

			for i, id := range ids {
				s.Equal(tc.expected[i], b.IsBanned(id), "ban %d", i)
			}
		})
	}
}

func (s *addrBookSuite) TestBanAfterExpiryIsNew() {
	b := s.newBook(nil)
	id := s.id()
	s.True(b.Ban(id, s.clock.Add(time.Hour)))
	s.False(b.Ban(id, s.clock.Add(2*time.Hour)))

	s.clock = s.clock.Add(3 * time.Hour)
	s.False(b.IsBanned(id))
	s.True(b.Ban(id, s.clock.Add(time.Hour)))
	s.True(b.IsBanned(id))
}

func (s *addrBookSuite) TestBanRules() {
	bootstrap, private := s.id(), s.id()

	testCases := []struct {
		name     string
		maxBans  int
		id       peer.ID
		bans     []time.Duration
		expected bool
	}{
		{name: "bans only extend", maxBans: 2, bans: []time.Duration{24 * time.Hour, time.Hour}, expected: true},
		{name: "bans extend at capacity", maxBans: 1, bans: []time.Duration{time.Hour, 3 * time.Hour}, expected: true},
		{name: "bootstrap peers are never banned", maxBans: 2, id: bootstrap, bans: []time.Duration{3 * time.Hour}},
		{name: "private peers are never banned", maxBans: 2, id: private, bans: []time.Duration{3 * time.Hour}},
		{name: "invalid peer IDs are never banned", maxBans: 2, id: peer.ID("abc"), bans: []time.Duration{3 * time.Hour}},
		{name: "peer IDs without a text form are never banned", maxBans: 2, id: sha1PeerID(), bans: []time.Duration{3 * time.Hour}},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			b := s.newBook(func(cfg *Config) {
				cfg.MaxBans = tc.maxBans
				cfg.Bootstrap = map[peer.ID]struct{}{bootstrap: {}}
				cfg.Private = map[peer.ID]struct{}{private: {}}
			})
			id := tc.id
			if id == "" {
				id = s.id()
				s.Require().NoError(b.AddGossiped(id, addr("8.8.8.8", 1), s.id()))
			}

			for _, d := range tc.bans {
				b.Ban(id, s.clock.Add(d))
			}
			s.clock = s.clock.Add(2 * time.Hour)

			_, stored := b.Get(id)
			s.False(stored)
			s.Equal(tc.expected, b.IsBanned(id))
			s.Equal(tc.expected, len(b.bans) == 1)
		})
	}
}

func (s *addrBookSuite) TestOutOfRangeTimesStayEncodable() {
	b := s.newBook(nil)
	cooled, banned := s.id(), s.id()
	s.Require().NoError(b.AddGossiped(cooled, addr("8.8.8.8", 1), s.id()))

	b.SetCooldown(cooled, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))
	b.Ban(banned, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))
	s.Require().NoError(b.Save())

	loaded := NewAddrBook(b.path, b.cfg)
	loaded.now = b.now
	s.Require().NoError(loaded.Load())
	s.True(loaded.InCooldown(cooled))
	s.True(loaded.IsBanned(banned))

	b.SetCooldown(cooled, time.Date(-5, 1, 1, 0, 0, 0, 0, time.UTC))
	s.Require().NoError(b.Save())
	s.False(b.InCooldown(cooled))

	b.SetCooldown(cooled, time.Date(0, 1, 1, 1, 0, 0, 0, time.UTC).In(time.FixedZone("west", -5*3600)))
	s.Require().NoError(b.Save())
}

func (s *addrBookSuite) TestLoadKeepsTheBestDuplicate() {
	id := s.id()
	content := fmt.Sprintf(`{"version":2,"entries":[
		{"id":%[1]q,"addr":"/ip4/9.9.9.9/udp/1/quic-v1","source":"inbound"},
		{"id":%[1]q,"addr":"/ip4/8.8.8.8/udp/1/quic-v1","source":"inbound","last_dialed_ok":"2025-12-31T00:00:00Z"}
	]}`, id)

	b := s.newBook(nil)
	s.Require().NoError(os.WriteFile(b.path, []byte(content), 0o600))
	s.Require().NoError(b.Load())

	e, ok := b.Get(id)
	s.Require().True(ok)
	s.True(addr("8.8.8.8", 1).Equal(e.Addr))
}

func (s *addrBookSuite) TestSaveLoad() {
	b := s.newBook(nil)
	gossiped, verified, banned, expired := s.id(), s.id(), s.id(), s.id()
	s.Require().NoError(b.AddGossiped(gossiped, addr("8.8.8.8", 1), s.id()))
	s.Require().NoError(b.AddVerified(verified, addr("9.9.9.9", 2), SourceInbound))
	b.MarkDialed(verified)
	b.Ban(banned, s.clock.Add(time.Hour))
	b.Ban(expired, s.clock.Add(time.Minute))
	s.clock = s.clock.Add(time.Minute)
	s.Require().NoError(b.Save())

	info, err := os.Stat(b.path)
	s.Require().NoError(err)
	s.Equal(os.FileMode(0o600), info.Mode().Perm())

	var file bookFile
	bz, err := os.ReadFile(b.path)
	s.Require().NoError(err)
	s.Require().NoError(json.Unmarshal(bz, &file))
	s.Len(file.Bans, 1)

	loaded := NewAddrBook(b.path, b.cfg)
	loaded.now = b.now
	s.Require().NoError(loaded.Load())

	s.Equal(2, loaded.Size())
	for _, id := range []peer.ID{gossiped, verified} {
		want, _ := b.Get(id)
		got, ok := loaded.Get(id)
		s.Require().True(ok)
		s.True(want.Addr.Equal(got.Addr))
		got.Addr = want.Addr
		s.Equal(want, got)
	}
	s.True(loaded.IsBanned(banned))

	late := NewAddrBook(b.path, b.cfg)
	late.now = func() time.Time { return s.clock.Add(2 * time.Hour) }
	s.Require().NoError(late.Load())
	s.Empty(late.bans)
}

func (s *addrBookSuite) TestLoadAppliesConfig() {
	b := s.newBook(func(cfg *Config) { cfg.AllowDuplicateIP = true })
	origin := s.id()
	best, verified, newest := s.id(), s.id(), s.id()
	s.Require().NoError(b.AddGossiped(best, addr("8.8.8.8", 1), origin))
	b.MarkDialed(best)
	for port := 2; port <= 3; port++ {
		s.clock = s.clock.Add(time.Second)
		s.Require().NoError(b.AddGossiped(s.id(), addr("8.8.8.8", port), origin))
	}
	s.clock = s.clock.Add(time.Second)
	s.Require().NoError(b.AddVerified(verified, addr("8.8.8.8", 4), SourceInbound))
	s.clock = s.clock.Add(time.Second)
	s.Require().NoError(b.AddGossiped(newest, addr("8.8.8.8", 5), origin))
	banned := s.id()
	b.Ban(banned, s.clock.Add(time.Hour))
	s.Require().NoError(b.Save())

	duplicates := func(cfg *Config) { cfg.AllowDuplicateIP = true }

	testCases := []struct {
		name     string
		malleate func(*Config)
		size     int
		kept     []peer.ID
		dropped  []peer.ID
		banned   bool
	}{
		{name: "same config", malleate: duplicates, size: 5, kept: []peer.ID{best, verified, newest}, banned: true},
		{name: "group cap keeps the best entries without reclaiming", size: 2, kept: []peer.ID{best, verified}, dropped: []peer.ID{newest}, banned: true},
		{
			name: "origin cap keeps the best entries",
			malleate: func(cfg *Config) {
				duplicates(cfg)
				cfg.MaxPerOrigin = 2
			},
			size:   3,
			kept:   []peer.ID{best, verified, newest},
			banned: true,
		},
		{
			name: "size cap keeps the best entry",
			malleate: func(cfg *Config) {
				duplicates(cfg)
				cfg.MaxSize = 1
			},
			size:    1,
			kept:    []peer.ID{best},
			dropped: []peer.ID{verified},
			banned:  true,
		},
		{
			name: "newly private peer",
			malleate: func(cfg *Config) {
				duplicates(cfg)
				cfg.Private = map[peer.ID]struct{}{best: {}}
			},
			size:    4,
			dropped: []peer.ID{best},
			banned:  true,
		},
		{
			name: "newly private origin",
			malleate: func(cfg *Config) {
				duplicates(cfg)
				cfg.Private = map[peer.ID]struct{}{origin: {}}
			},
			size:    1,
			kept:    []peer.ID{verified},
			dropped: []peer.ID{best, newest},
			banned:  true,
		},
		{
			name: "ban of a newly bootstrap peer",
			malleate: func(cfg *Config) {
				duplicates(cfg)
				cfg.Bootstrap = map[peer.ID]struct{}{banned: {}}
			},
			size: 5,
		},
		{
			name: "ban cap",
			malleate: func(cfg *Config) {
				duplicates(cfg)
				cfg.MaxBans = 0
			},
			size: 5,
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			cfg := DefaultConfig()
			if tc.malleate != nil {
				tc.malleate(&cfg)
			}
			loaded := NewAddrBook(b.path, cfg)
			loaded.now = b.now

			s.Require().NoError(loaded.Load())

			s.Equal(tc.size, loaded.Size())
			for _, id := range tc.kept {
				_, ok := loaded.Get(id)
				s.True(ok)
			}
			for _, id := range tc.dropped {
				_, ok := loaded.Get(id)
				s.False(ok)
			}
			s.Equal(tc.banned, loaded.IsBanned(banned))
		})
	}
}

func (s *addrBookSuite) TestLoadDoesNotReclaim() {
	b := s.newBook(func(cfg *Config) { cfg.AllowDuplicateIP = true })
	ids := make([]peer.ID, 3)
	for i := range ids {
		ids[i] = s.id()
		s.Require().NoError(b.AddGossiped(ids[i], addr("8.8.8.8", i+1), s.id()))
		s.markAttempts(b, ids[i], 3)
		s.clock = s.clock.Add(time.Second)
	}
	s.clock = s.clock.Add(2 * recentAttempt)
	s.Require().NoError(b.Save())

	loaded := NewAddrBook(b.path, DefaultConfig())
	loaded.now = b.now
	s.Require().NoError(loaded.Load())

	for i, expected := range []bool{false, true, true} {
		_, kept := loaded.Get(ids[i])
		s.Equal(expected, kept, "entry %d", i)
	}
}

func (s *addrBookSuite) TestIOErrors() {
	missingDir := NewAddrBook(filepath.Join(s.T().TempDir(), "missing", "addrbook.json"), DefaultConfig())
	s.Require().Error(missingDir.Save())

	directory := NewAddrBook(s.T().TempDir(), DefaultConfig())
	s.Require().ErrorContains(directory.Load(), "read address book")
}

func (s *addrBookSuite) TestEvictionTiesBreakByPeerID() {
	for range 10 {
		b := s.newBook(func(cfg *Config) { cfg.MaxSize = 2 })
		first, second := s.id(), s.id()
		s.Require().NoError(b.AddGossiped(first, addr("1.1.1.1", 1), s.id()))
		s.Require().NoError(b.AddGossiped(second, addr("2.2.2.2", 1), s.id()))

		s.Require().NoError(b.AddGossiped(s.id(), addr("3.3.3.3", 1), s.id()))

		_, keptFirst := b.Get(first)
		s.Equal(first > second, keptFirst)
	}
}

func (s *addrBookSuite) TestLoadFiles() {
	id := s.id()

	testCases := []struct {
		name     string
		content  string
		errorMsg string
	}{
		{name: "missing file"},
		{name: "entry without address", content: fmt.Sprintf(`{"version":2,"entries":[{"id":%q,"source":"gossip","origin":%q}]}`, id, id)},
		{name: "entry with unknown source", content: fmt.Sprintf(`{"version":2,"entries":[{"id":%q,"addr":"/ip4/8.8.8.8/udp/1/quic-v1","source":"x"}]}`, id)},
		{name: "entry for a banned peer", content: fmt.Sprintf(`{"version":2,"entries":[{"id":%q,"addr":"/ip4/8.8.8.8/udp/1/quic-v1","source":"inbound"}],"bans":[{"id":%q,"until":"9999-01-01T00:00:00Z"}]}`, id, id)},
		{name: "gossip entry without origin", content: fmt.Sprintf(`{"version":2,"entries":[{"id":%q,"addr":"/ip4/8.8.8.8/udp/1/quic-v1","source":"gossip"}]}`, id)},
		{name: "classic book", content: `{"key":"abc","addrs":[]}`, errorMsg: "address book version 0, want 2"},
		{name: "other version", content: `{"version":3}`, errorMsg: "address book version 3, want 2"},
		{name: "corrupt file", content: `{`, errorMsg: "decode address book"},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			b := s.newBook(nil)
			if tc.content != "" {
				s.Require().NoError(os.WriteFile(b.path, []byte(tc.content), 0o600))
			}

			err := b.Load()
			if tc.errorMsg != "" {
				s.Require().ErrorContains(err, tc.errorMsg)
				return
			}
			s.Require().NoError(err)
			s.Zero(b.Size())
		})
	}
}

func (s *addrBookSuite) TestConcurrentUse() {
	b := s.newBook(func(cfg *Config) { cfg.Strict = false })
	b.now = time.Now

	ids := make([][]peer.ID, 8)
	for i := range ids {
		ids[i] = make([]peer.ID, 50)
		for j := range ids[i] {
			ids[i][j] = s.id()
		}
	}
	origin := s.id()

	var wg sync.WaitGroup
	for i := range ids {
		wg.Go(func() {
			for j, id := range ids[i] {
				_ = b.AddGossiped(id, addr(fmt.Sprintf("10.%d.%d.1", i, j), 1), origin)
				_ = b.AddVerified(id, addr(fmt.Sprintf("10.%d.%d.2", i, j), 1), SourceInbound)
				b.MarkAttempt(id)
				b.MarkDialed(id)
				b.MarkGood(id)
				_, _ = b.Get(id)
				_ = b.InCooldown(id)
				if j%10 == 0 {
					b.Ban(id, time.Now().Add(time.Hour))
					_ = b.IsBanned(id)
					s.NoError(b.Save())
				}
			}
		})
	}
	wg.Wait()

	b.mu.Lock()
	defer b.mu.Unlock()
	s.Positive(len(b.entries))
	groups := make(map[string]int)
	for _, e := range b.entries {
		groups[ipGroup(e.ip)]++
	}
	for group, n := range groups {
		s.LessOrEqual(n, b.cfg.MaxPerGroup, group)
	}
}

func (s *addrBookSuite) TestVerifiedInboundAddr() {
	listen := []ma.Multiaddr{ma.StringCast("/ip4/8.8.8.8/tcp/26656"), addr("10.0.0.1", 26656), addr("8.8.8.8", 26656), addr("2001:4860::1", 26656)}

	testCases := []struct {
		name     string
		remote   ma.Multiaddr
		expected ma.Multiaddr
	}{
		{name: "matching IPv4", remote: addr("8.8.8.8", 26656), expected: addr("8.8.8.8", 26656)},
		{name: "matching IPv6", remote: addr("2001:4860::1", 26656), expected: addr("2001:4860::1", 26656)},
		{name: "other port", remote: addr("8.8.8.8", 40000)},
		{name: "other IP", remote: addr("9.9.9.9", 26656)},
		{name: "malformed remote", remote: ma.StringCast("/ip4/8.8.8.8/tcp/26656")},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			got, ok := VerifiedInboundAddr(tc.remote, listen)
			if tc.expected == nil {
				s.False(ok)
				return
			}
			s.Require().True(ok)
			s.True(tc.expected.Equal(got))
		})
	}
}
