// Package lp2p implements auxiliary functions for go-libp2p integration in CometBFT.
// The name is chosen to avoid conflicts with the p2p package.
package lp2p

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/cometbft/cometbft/config"
	cmcrypto "github.com/cometbft/cometbft/crypto"
	"github.com/cometbft/cometbft/libs/log"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/connmgr"
	"github.com/libp2p/go-libp2p/core/control"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/libp2p/go-libp2p/p2p/protocol/ping"
	quic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	multiaddr "github.com/multiformats/go-multiaddr"
)

// Host is a wrapper around the libp2p host.
// Note that host should NOT be responsible for high-level peer management
// as it's Switch's responsibility.
type Host struct {
	host.Host

	config config.LibP2PConfig

	// bootstrapPeers are initial peers specified in the address book
	bootstrapPeers map[peer.ID]BootstrapPeer

	connGater *ConnGater

	logger log.Logger

	peerFailureHandlers []func(id peer.ID, err error)
}

// BootstrapPeer initial peers to connect to
type BootstrapPeer struct {
	AddrInfo      peer.AddrInfo
	Private       bool
	Persistent    bool
	Unconditional bool
}

// TransportQUIC quic transport.
// @see https://docs.libp2p.io/concepts/transports/quic
const TransportQUIC = "quic-v1"

const connMgrBootstrapTag = "bootstrap"

// BanChecker reports peers that must not connect.
type BanChecker interface {
	IsBanned(id peer.ID) bool
}

// NewHost Host constructor.
func NewHost(config *config.P2PConfig, nodeKey cmcrypto.PrivKey, logger log.Logger) (*Host, error) {
	if !config.LibP2PEnabled() {
		return nil, fmt.Errorf("libp2p is disabled")
	}

	if err := config.LibP2PConfig.ValidateBasic(); err != nil {
		return nil, fmt.Errorf("invalid libp2p config: %w", err)
	}

	privateKey, err := PrivateKeyFromCosmosKey(nodeKey)
	if err != nil {
		return nil, fmt.Errorf("failed to convert private key to libp2p: %w", err)
	}

	listenAddr, err := AddressToMultiAddr(config.ListenAddress, TransportQUIC)
	if err != nil {
		return nil, fmt.Errorf("failed to convert %q to multiaddr: %w", config.ListenAddress, err)
	}

	bootstrapPeers, err := BootstrapPeersFromConfig(config.LibP2PConfig)
	switch {
	case err != nil:
		return nil, fmt.Errorf("failed to decode bootstrap peers: %w", err)
	case len(bootstrapPeers) == 0:
		logger.Info("No bootstrap peers provided in the config")
	}

	// host will be set later
	connGater, connGaterEnabled := ConnectionGaterFromConfig(config, bootstrapPeers)

	resourceManager, _, err := ResourceManagerFromConfig(config.LibP2PConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource manager: %w", err)
	}

	// todo: add support for libp2p.BandwidthReporter()
	opts := []libp2p.Option{
		libp2p.Identity(privateKey),
		libp2p.ListenAddrs(listenAddr),
		libp2p.UserAgent("cometbft"),
		libp2p.Ping(true),
		libp2p.Transport(quic.NewTransport),
		libp2p.ResourceManager(resourceManager),
		// Gossiped dead addresses must not make the swarm refuse dials to trusted peers.
		libp2p.UDPBlackHoleSuccessCounter(nil),
		libp2p.IPv6BlackHoleSuccessCounter(nil),
	}

	if connGaterEnabled {
		opts = append(opts, libp2p.ConnectionGater(connGater))
	}

	// We listen on `listenAddr` but advertise `externalAddr` to peers
	if config.ExternalAddress != "" {
		externalAddr, err := AddressToMultiAddr(config.ExternalAddress, TransportQUIC)
		if err != nil {
			return nil, fmt.Errorf("failed to convert %q to multiaddr: %w", config.ExternalAddress, err)
		}

		opts = append(opts, withAddressFactory(externalAddr))
	}

	host, err := libp2p.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create libp2p host: %w", err)
	}

	h := &Host{
		Host:           host,
		config:         config.LibP2PConfig,
		bootstrapPeers: bootstrapPeers,
		logger:         logger,
	}

	if connGaterEnabled {
		h.connGater = connGater
		connGater.SetHost(h)
	}

	for id := range bootstrapPeers {
		host.ConnManager().Protect(id, connMgrBootstrapTag)
	}

	return h, nil
}

func (h *Host) AddrInfo() peer.AddrInfo {
	return peer.AddrInfo{ID: h.ID(), Addrs: h.Addrs()}
}

func (h *Host) BootstrapPeers() map[peer.ID]BootstrapPeer {
	return h.bootstrapPeers
}

func (h *Host) BootstrapPeer(id peer.ID) (BootstrapPeer, bool) {
	bp, ok := h.bootstrapPeers[id]
	return bp, ok
}

func (h *Host) Logger() log.Logger {
	return h.logger
}

// SetBanChecker makes the connection gater refuse banned peers.
func (h *Host) SetBanChecker(bans BanChecker) {
	if h.connGater != nil && bans != nil {
		h.connGater.bans.Store(&banChecker{bans})
	}
}

// Ping pings peers and logs RTT latency (blocking)
// Keep in might that ping service might be disabled on the counterparty's side.
func (h *Host) Ping(ctx context.Context, addrInfo peer.AddrInfo) (time.Duration, error) {
	res := <-ping.Ping(ctx, h, addrInfo.ID)

	return res.RTT, res.Error
}

func (h *Host) AddPeerFailureHandler(handler func(id peer.ID, err error)) {
	h.peerFailureHandlers = append(h.peerFailureHandlers, handler)
}

// EmitPeerFailure emits a peer failure event to all registered handlers.
// This semantic is over host.eventBus for simplicity.
func (h *Host) EmitPeerFailure(id peer.ID, err error) {
	for _, handler := range h.peerFailureHandlers {
		go handler(id, err)
	}
}

func BootstrapPeersFromConfig(config config.LibP2PConfig) (map[peer.ID]BootstrapPeer, error) {
	peers := make(map[peer.ID]BootstrapPeer, len(config.BootstrapPeers))

	for _, bp := range config.BootstrapPeers {
		addr, err := AddrInfoFromHostAndID(bp.Host, bp.ID)
		if err != nil {
			return nil, fmt.Errorf("[%s, %s]: %w", bp.Host, bp.ID, err)
		}

		if _, ok := peers[addr.ID]; ok {
			continue
		}

		peers[addr.ID] = BootstrapPeer{
			AddrInfo:      addr,
			Private:       bp.Private,
			Persistent:    bp.Persistent,
			Unconditional: bp.Unconditional,
		}
	}

	return peers, nil
}

// PEXPeerStreams bounds PEX streams per peer and direction unless limits are
// disabled. There is no PEX-specific global cap, so a few peers cannot starve the rest.
const PEXPeerStreams = 4

// systemConnsPerPeer is the per-peer connection budget used to size the
// system-wide connection limit in custom limits mode.
const systemConnsPerPeer = 4

// ResourceManagerFromConfig creates a resource manager from the given config.
func ResourceManagerFromConfig(cfg config.LibP2PConfig) (network.ResourceManager, rcmgr.Limiter, error) {
	if cfg.Limits.Mode == config.LibP2PLimitsModeDisabled {
		return &network.NullResourceManager{}, nil, nil
	}

	// this is what lib-p2p does by default:
	// mem limit: 1/8th of total memory, max 128MB, min 1GB (see defaults.AutoScale())
	defaults := rcmgr.DefaultLimits

	// cap limits for default lib-p2p protocols (identity, ping, ...)
	libp2p.SetDefaultServiceLimits(&defaults)

	defaults.AddProtocolPeerLimit(
		ProtocolID(PEXChannel),
		rcmgr.BaseLimit{
			StreamsInbound:  PEXPeerStreams,
			StreamsOutbound: PEXPeerStreams,
			Streams:         2 * PEXPeerStreams,
			Memory:          1 << 20,
		},
		rcmgr.BaseLimitIncrease{},
	)

	if cfg.Limits.Mode == config.LibP2PLimitsModeDefault {
		limiter := rcmgr.NewFixedLimiter(defaults.AutoScale())
		mgr, err := rcmgr.NewResourceManager(limiter)

		return mgr, limiter, err
	}

	if cfg.Limits.Mode == config.LibP2PLimitsModeCustom {
		var (
			partialDefaults = defaults.AutoScale().ToPartialLimitConfig()
			limits          = rcmgr.InfiniteLimits.ToPartialLimitConfig()
			maxPeers        = rcmgr.LimitVal(cfg.Limits.MaxPeers)
			maxPeerStreams  = rcmgr.LimitVal(cfg.Limits.MaxPeerStreams)
		)

		// 1. copy defaults for built-in services/protocols
		limits.Service = partialDefaults.Service
		limits.ServicePeer = partialDefaults.ServicePeer
		limits.Protocol = partialDefaults.Protocol
		limits.ProtocolPeer = partialDefaults.ProtocolPeer

		// 2. also copy sane default conns for peers
		limits.PeerDefault.Conns = partialDefaults.PeerDefault.Conns
		limits.PeerDefault.ConnsInbound = partialDefaults.PeerDefault.ConnsInbound
		limits.PeerDefault.ConnsOutbound = partialDefaults.PeerDefault.ConnsOutbound

		// 2.1 limit max system connections to (max peers * conns per peer).
		// Peers normally hold a single multiplexed connection; the small
		// constant leaves room for a reconnect while the old one drains.
		limits.System.Conns = maxPeers * systemConnsPerPeer

		// 3. set max streams
		// https://github.com/libp2p/go-libp2p/blob/da810a1/p2p/host/resource-manager/scope.go#L168
		limits.PeerDefault.Streams = maxPeerStreams
		limits.PeerDefault.StreamsInbound = maxPeerStreams
		limits.PeerDefault.StreamsOutbound = maxPeerStreams

		limiter := rcmgr.NewFixedLimiter(limits.Build(rcmgr.InfiniteLimits))
		mgr, err := rcmgr.NewResourceManager(limiter)

		return mgr, limiter, err
	}

	return nil, nil, fmt.Errorf("unknown limits mode: %q", cfg.Limits.Mode)
}

// ConnGater refuses banned peers, inbound peers beyond
// `p2p.max_num_inbound_peers` when PEX is enabled, and, when
// `lp2p.limits.mode = "custom"`, peers beyond `lp2p.limits.max_peers`.
// Bootstrap peers are never gated.
//
// The host is injected after host creation because libp2p requires the
// connection gater option during `libp2p.New(...)`, before the host exists.
type ConnGater struct {
	host           atomic.Pointer[Host]
	bans           atomic.Pointer[banChecker]
	maxPeers       int // 0 means no cap
	limitInbound   bool
	maxInbound     int // with limitInbound, 0 refuses all non-bootstrap inbound peers
	bootstrapPeers map[peer.ID]BootstrapPeer
}

type banChecker struct{ BanChecker }

var _ connmgr.ConnectionGater = (*ConnGater)(nil)

// ConnectionGaterFromConfig creates a connection gater from the given config or returns false if disabled.
func ConnectionGaterFromConfig(cfg *config.P2PConfig, bootstrapPeers map[peer.ID]BootstrapPeer) (*ConnGater, bool) {
	custom := cfg.LibP2PConfig.Limits.Mode == config.LibP2PLimitsModeCustom
	pex := cfg.LibP2PConfig.PEX.Enabled
	if !custom && !pex {
		return nil, false
	}

	gater := &ConnGater{bootstrapPeers: bootstrapPeers}
	if custom {
		gater.maxPeers = cfg.LibP2PConfig.Limits.MaxPeers
	}
	if pex {
		gater.limitInbound = true
		gater.maxInbound = cfg.MaxNumInboundPeers
	}

	return gater, true
}

// SetHost sets the host for the connection gater. The host is injected after creation
// because libp2p requires the connection gater option during libp2p.New, before the host exists.
func (c *ConnGater) SetHost(host *Host) { c.host.Store(host) }

// InterceptAccept allows every connection: the peer ID is not known yet,
// so peer checks run in InterceptSecured.
func (c *ConnGater) InterceptAccept(network.ConnMultiaddrs) bool {
	return true
}

func (c *ConnGater) InterceptAddrDial(pid peer.ID, _ multiaddr.Multiaddr) bool {
	return c.allowPeer(pid, "InterceptAddrDial", false)
}

func (c *ConnGater) InterceptPeerDial(pid peer.ID) bool {
	return c.allowPeer(pid, "InterceptPeerDial", false)
}

// InterceptSecured gates inbound connections.
// Outbound connections were already checked when dialing.
func (c *ConnGater) InterceptSecured(dir network.Direction, pid peer.ID, _ network.ConnMultiaddrs) bool {
	if dir == network.DirOutbound {
		return true
	}

	return c.allowPeer(pid, "InterceptSecured", true)
}

func (c *ConnGater) InterceptUpgraded(network.Conn) (allow bool, reason control.DisconnectReason) {
	return true, 0
}

func (c *ConnGater) allowPeer(pid peer.ID, caller string, inbound bool) bool {
	if _, ok := c.bootstrapPeers[pid]; ok {
		return true
	}

	h := c.host.Load()
	if h == nil {
		return false
	}

	if bans := c.bans.Load(); bans != nil && bans.IsBanned(pid) {
		h.logger.Debug("Rejecting banned peer", "caller", caller, "peer_id", pid.String())
		return false
	}

	if inbound && !c.allowInbound(h) {
		h.logger.Debug("Rejecting peer due to max inbound peers limit", "caller", caller, "peer_id", pid.String(), "max_inbound_peers", c.maxInbound)
		return false
	}

	if c.maxPeers == 0 {
		return true
	}

	current := len(h.Network().Peers())
	if current < c.maxPeers {
		return true
	}

	h.logger.Info(
		"Rejecting peer due to max peers limit",
		"caller", caller,
		"peer_id", pid.String(),
		"current_peers", current,
		"max_peers", c.maxPeers,
	)

	return false
}

func (c *ConnGater) allowInbound(h *Host) bool {
	return !c.limitInbound || (c.maxInbound > 0 && c.inboundPeers(h.Network().Conns()) < c.maxInbound)
}

// Neither a bootstrap peer nor a peer we dialed takes an inbound slot.
func (c *ConnGater) inboundPeers(conns []network.Conn) int {
	hasOutbound := make(map[peer.ID]bool)
	for _, conn := range conns {
		id := conn.RemotePeer()
		hasOutbound[id] = hasOutbound[id] || isOutbound(conn)
	}

	inbound := 0
	for id, out := range hasOutbound {
		if _, bootstrap := c.bootstrapPeers[id]; !out && !bootstrap {
			inbound++
		}
	}

	return inbound
}
