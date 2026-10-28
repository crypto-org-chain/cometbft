package pex

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/crypto/ed25519"
	"github.com/cometbft/cometbft/libs/log"
	"github.com/cometbft/cometbft/lp2p"
	"github.com/cometbft/cometbft/p2p"
	"github.com/cometbft/cometbft/test/utils"
	p2ptest "github.com/libp2p/go-libp2p/core/test"
	"github.com/stretchr/testify/require"
)

func TestPEXBetweenSwitches(t *testing.T) {
	ports := utils.GetFreePorts(t, 2)

	server, serverBook := startPEXSwitch(t, ports[0], nil)
	known := p2ptest.RandPeerIDFatal(t)
	require.NoError(t, serverBook.AddVerified(known, addr("8.8.8.8", 26656), SourceInbound))

	client, clientBook := startPEXSwitch(t, ports[1], []config.LibP2PBootstrapPeer{{
		Host: fmt.Sprintf("127.0.0.1:%d", ports[0]),
		ID:   server.ID().String(),
	}})

	require.Eventually(t, func() bool {
		e, ok := clientBook.Get(known)
		return ok && e.Origin == server.ID()
	}, 5*time.Second, 20*time.Millisecond)

	require.Eventually(t, func() bool {
		e, ok := serverBook.Get(client.ID())
		return ok && e.Source == SourceInbound
	}, 5*time.Second, 20*time.Millisecond)
}

func startPEXSwitch(t *testing.T, port int, bootstrap []config.LibP2PBootstrapPeer) (*lp2p.Host, *AddrBook) {
	h, book, _ := startPEXSwitchWith(t, port, bootstrap, nil)
	return h, book
}

func startPEXSwitchWith(
	t *testing.T,
	port int,
	bootstrap []config.LibP2PBootstrapPeer,
	malleate func(*Reactor),
) (*lp2p.Host, *AddrBook, *lp2p.Switch) {
	t.Helper()

	cfg := config.DefaultP2PConfig()
	cfg.RootDir = t.TempDir()
	cfg.ListenAddress = fmt.Sprintf("127.0.0.1:%d", port)
	cfg.ExternalAddress = cfg.ListenAddress
	cfg.LibP2PConfig.Enabled = true
	cfg.LibP2PConfig.BootstrapPeers = bootstrap

	logger := log.NewNopLogger()
	host, err := lp2p.NewHost(cfg, ed25519.GenPrivKey(), logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = host.Close() })

	bookCfg := DefaultConfig()
	bookCfg.Self = host.ID()
	bookCfg.Strict = false
	book := NewAddrBook(filepath.Join(cfg.RootDir, "addrbook.json"), bookCfg)

	reactor := NewReactor(book, host, 10, p2p.NopMetrics())
	if malleate != nil {
		malleate(reactor)
	}
	sw, err := lp2p.NewSwitch(nil, host, []lp2p.SwitchReactor{{Name: "PEX", Reactor: reactor}}, p2p.NopMetrics(), logger)
	require.NoError(t, err)
	require.NoError(t, sw.Start())
	t.Cleanup(func() { _ = sw.Stop() })

	return host, book, sw
}

func TestPEXDialsDiscoveredPeer(t *testing.T) {
	ports := utils.GetFreePorts(t, 3)

	target, _, _ := startPEXSwitchWith(t, ports[2], nil, nil)
	server, serverBook, _ := startPEXSwitchWith(t, ports[0], nil, nil)
	require.NoError(t, serverBook.AddVerified(target.ID(), target.Addrs()[0], SourceInbound))

	_, clientBook, client := startPEXSwitchWith(t, ports[1], []config.LibP2PBootstrapPeer{{
		Host: fmt.Sprintf("127.0.0.1:%d", ports[0]),
		ID:   server.ID().String(),
	}}, func(r *Reactor) { r.dialEvery = 50 * time.Millisecond })

	require.Eventually(t, func() bool {
		p := client.Peers().Get(p2p.ID(target.ID().String()))
		e, ok := clientBook.Get(target.ID())
		return p != nil && p.IsOutbound() && ok && e.Source == SourceDialed && !e.LastDialedOK.IsZero()
	}, 10*time.Second, 50*time.Millisecond)
}

func TestPEXForgetsAddressOfAnotherPeer(t *testing.T) {
	ports := utils.GetFreePorts(t, 2)
	other, _, _ := startPEXSwitchWith(t, ports[0], nil, nil)
	claimed := p2ptest.RandPeerIDFatal(t)

	_, book, _ := startPEXSwitchWith(t, ports[1], nil, func(r *Reactor) { r.dialEvery = 50 * time.Millisecond })
	require.NoError(t, book.AddVerified(claimed, other.Addrs()[0], SourceInbound))

	require.Eventually(t, func() bool {
		_, ok := book.Get(claimed)
		return !ok
	}, 10*time.Second, 50*time.Millisecond)
}
