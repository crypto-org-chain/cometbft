package node

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbm "github.com/cometbft/cometbft-db"

	"github.com/cometbft/cometbft/abci/example/kvstore"
	cfg "github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/crypto/ed25519"
	"github.com/cometbft/cometbft/evidence"
	"github.com/cometbft/cometbft/internal/test"
	"github.com/cometbft/cometbft/libs/log"
	cmtrand "github.com/cometbft/cometbft/libs/rand"
	"github.com/cometbft/cometbft/lp2p"
	lp2ppex "github.com/cometbft/cometbft/lp2p/pex"
	mempl "github.com/cometbft/cometbft/mempool"
	"github.com/cometbft/cometbft/p2p"
	"github.com/cometbft/cometbft/p2p/conn"
	p2pmock "github.com/cometbft/cometbft/p2p/mock"
	"github.com/cometbft/cometbft/privval"
	"github.com/cometbft/cometbft/proxy"
	sm "github.com/cometbft/cometbft/state"
	"github.com/cometbft/cometbft/store"
	"github.com/cometbft/cometbft/test/utils"
	"github.com/cometbft/cometbft/types"
	cmttime "github.com/cometbft/cometbft/types/time"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

func TestNodeStartStop(t *testing.T) {
	config := newNodeTestConfig(t, "node_node_test")
	defer os.RemoveAll(config.RootDir)

	// create & start node
	n, err := DefaultNewNode(config, log.TestingLogger())
	require.NoError(t, err)
	err = n.Start()
	require.NoError(t, err)

	t.Logf("Started node %v", n.sw.NodeInfo())

	// wait for the node to produce a block
	blocksSub, err := n.EventBus().Subscribe(context.Background(), "node_test", types.EventQueryNewBlock)
	require.NoError(t, err)
	select {
	case <-blocksSub.Out():
	case <-blocksSub.Canceled():
		t.Fatal("blocksSub was canceled")
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the node to produce a block")
	}

	// stop the node
	go func() {
		err = n.Stop()
		require.NoError(t, err)
	}()

	select {
	case <-n.Quit():
	case <-time.After(5 * time.Second):
		pid := os.Getpid()
		p, err := os.FindProcess(pid)
		if err != nil {
			panic(err)
		}
		err = p.Signal(syscall.SIGABRT)
		fmt.Println(err)
		t.Fatal("timed out waiting for shutdown")
	}
}

func TestSplitAndTrimEmpty(t *testing.T) {
	testCases := []struct {
		s        string
		sep      string
		cutset   string
		expected []string
	}{
		{"a,b,c", ",", " ", []string{"a", "b", "c"}},
		{" a , b , c ", ",", " ", []string{"a", "b", "c"}},
		{" a, b, c ", ",", " ", []string{"a", "b", "c"}},
		{" a, ", ",", " ", []string{"a"}},
		{"   ", ",", " ", []string{}},
	}

	for _, tc := range testCases {
		assert.Equal(t, tc.expected, splitAndTrimEmpty(tc.s, tc.sep, tc.cutset), "%s", tc.s)
	}
}

func TestWarnUnsupportedLibP2PSettings(t *testing.T) {
	testCases := []struct {
		name     string
		malleate func(*cfg.P2PConfig)
		expected string
		hint     bool
	}{
		{name: "defaults", malleate: func(*cfg.P2PConfig) {}},
		{
			name: "blank lists",
			malleate: func(c *cfg.P2PConfig) {
				c.Seeds = " , "
				c.PersistentPeers = " "
			},
		},
		{
			name: "classic peer settings",
			malleate: func(c *cfg.P2PConfig) {
				c.Seeds = "id@host:26656"
				c.PersistentPeers = "id@host:26656"
				c.UnconditionalPeerIDs = "id"
				c.PrivatePeerIDs = "id"
				c.SeedMode = true
				c.LibP2PConfig.Enabled = true
				c.LibP2PConfig.PEX.Enabled = true
			},
			expected: "settings=\"[p2p.seeds p2p.persistent_peers p2p.unconditional_peer_ids p2p.private_peer_ids p2p.seed_mode]\"",
			hint:     true,
		},
		{
			name: "private peers already set for libp2p",
			malleate: func(c *cfg.P2PConfig) {
				c.PrivatePeerIDs = "id"
				c.LibP2PConfig.Enabled = true
				c.LibP2PConfig.PEX.Enabled = true
				c.LibP2PConfig.PrivatePeerIDs = []string{"id"}
			},
			expected: "settings=[p2p.private_peer_ids]",
		},
		{
			name:     "private peers without peer exchange",
			malleate: func(c *cfg.P2PConfig) { c.PrivatePeerIDs = "id" },
			expected: "settings=[p2p.private_peer_ids]",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg.DefaultP2PConfig()
			tc.malleate(c)

			var buf bytes.Buffer
			warnUnsupportedLibP2PSettings(c, log.NewTMLogger(&buf))

			if tc.expected == "" {
				assert.Empty(t, buf.String())
				return
			}
			assert.Contains(t, buf.String(), tc.expected)
			assert.Equal(t, tc.hint, strings.Contains(buf.String(), "p2p.libp2p.private_peer_ids"))
		})
	}
}

func TestCreateLibP2PPEXReactor(t *testing.T) {
	privateID, bootstrapID, privateBootstrapID := randomPeerID(t), randomPeerID(t), randomPeerID(t)
	public := func(port int) multiaddr.Multiaddr {
		return multiaddr.StringCast(fmt.Sprintf("/ip4/8.8.8.8/udp/%d/quic-v1", port))
	}

	testCases := []struct {
		name     string
		malleate func(*cfg.P2PConfig)
		check    func(t *testing.T, book *lp2ppex.AddrBook, host *lp2p.Host, connected peer.ID)
		noSwitch bool
	}{
		{
			name: "never stores self, private or bootstrap peers",
			check: func(t *testing.T, book *lp2ppex.AddrBook, host *lp2p.Host, _ peer.ID) {
				origin := randomPeerID(t)
				require.ErrorIs(t, book.AddGossiped(privateID, public(1), origin), lp2ppex.ErrPrivate)
				require.ErrorIs(t, book.AddGossiped(privateBootstrapID, public(1), origin), lp2ppex.ErrPrivate)
				require.ErrorIs(t, book.AddGossiped(bootstrapID, public(1), origin), lp2ppex.ErrBootstrap)
				require.ErrorIs(t, book.AddGossiped(host.ID(), public(1), origin), lp2ppex.ErrSelf)
				require.ErrorIs(t, book.AddGossiped(randomPeerID(t), public(1), privateID), lp2ppex.ErrPrivate)
				require.NoError(t, book.AddGossiped(randomPeerID(t), public(1), bootstrapID))
			},
		},
		{
			name: "strict book",
			check: func(t *testing.T, book *lp2ppex.AddrBook, _ *lp2p.Host, _ peer.ID) {
				local := multiaddr.StringCast("/ip4/127.0.0.1/udp/26656/quic-v1")
				require.ErrorIs(t, book.AddGossiped(randomPeerID(t), local, randomPeerID(t)), lp2ppex.ErrUnroutable)
			},
		},
		{
			name:     "non-strict book",
			malleate: func(c *cfg.P2PConfig) { c.AddrBookStrict = false },
			check: func(t *testing.T, book *lp2ppex.AddrBook, _ *lp2p.Host, _ peer.ID) {
				local := multiaddr.StringCast("/ip4/127.0.0.1/udp/26656/quic-v1")
				require.NoError(t, book.AddGossiped(randomPeerID(t), local, randomPeerID(t)))
			},
		},
		{
			name:     "duplicate IPs allowed",
			malleate: func(c *cfg.P2PConfig) { c.AllowDuplicateIP = true },
			check: func(t *testing.T, book *lp2ppex.AddrBook, _ *lp2p.Host, _ peer.ID) {
				for port := 1; port <= 3; port++ {
					require.NoError(t, book.AddGossiped(randomPeerID(t), public(port), randomPeerID(t)))
				}
			},
		},
		{
			name: "connected peers keep their group slot",
			check: func(t *testing.T, book *lp2ppex.AddrBook, _ *lp2p.Host, connected peer.ID) {
				other := randomPeerID(t)
				require.NoError(t, book.AddGossiped(connected, public(1), randomPeerID(t)))
				require.NoError(t, book.AddGossiped(other, public(2), randomPeerID(t)))

				require.NoError(t, book.AddVerified(randomPeerID(t), public(3), lp2ppex.SourceInbound))

				_, keptConnected := book.Get(connected)
				_, keptOther := book.Get(other)
				require.True(t, keptConnected)
				require.False(t, keptOther)
			},
		},
		{
			name:     "nothing is connected before the switch is set",
			noSwitch: true,
			check: func(t *testing.T, book *lp2ppex.AddrBook, _ *lp2p.Host, connected peer.ID) {
				require.NoError(t, book.AddGossiped(connected, public(1), randomPeerID(t)))
				require.NoError(t, book.AddGossiped(randomPeerID(t), public(2), randomPeerID(t)))

				require.NoError(t, book.AddVerified(randomPeerID(t), public(3), lp2ppex.SourceInbound))

				_, keptConnected := book.Get(connected)
				require.False(t, keptConnected)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			config := cfg.TestConfig().SetRoot(t.TempDir())
			config.P2P.ListenAddress = fmt.Sprintf("127.0.0.1:%d", utils.GetFreePorts(t, 1)[0])
			config.P2P.AddrBookStrict = true
			config.P2P.AllowDuplicateIP = false
			config.P2P.LibP2PConfig.Enabled = true
			config.P2P.LibP2PConfig.PrivatePeerIDs = []string{privateID.String()}
			config.P2P.LibP2PConfig.BootstrapPeers = []cfg.LibP2PBootstrapPeer{
				{Host: "127.0.0.1:1", ID: bootstrapID.String()},
				{Host: "127.0.0.1:2", ID: privateBootstrapID.String(), Private: true},
			}
			if tc.malleate != nil {
				tc.malleate(config.P2P)
			}
			host, err := lp2p.NewHost(config.P2P, ed25519.GenPrivKey(), log.NewNopLogger())
			require.NoError(t, err)
			t.Cleanup(func() { _ = host.Close() })

			connected := randomPeerID(t)
			peers := p2p.NewPeerSet()
			require.NoError(t, peers.Add(&idPeer{Peer: p2pmock.NewPeer(nil), id: p2p.ID(connected.String())}))

			reactor, book, err := createLibP2PPEXReactor(config.P2P, host, log.NewNopLogger())
			require.NoError(t, err)
			if !tc.noSwitch {
				reactor.SetSwitch(&peersSwitch{peers: peers})
			}

			tc.check(t, book, host, connected)
			require.NoError(t, book.Save())
			require.FileExists(t, config.P2P.LibP2PAddrBookFile())
		})
	}
}

func TestNodeClosesHostWhenPEXSetupFails(t *testing.T) {
	config := newNodeTestConfig(t, "node_libp2p_pex_fail_test")
	t.Cleanup(func() { _ = os.RemoveAll(config.RootDir) })
	port := utils.GetFreePorts(t, 1)[0]
	config.P2P.ListenAddress = fmt.Sprintf("127.0.0.1:%d", port)
	config.P2P.LibP2PConfig.Enabled = true
	config.P2P.LibP2PConfig.PEX.Enabled = true
	config.P2P.LibP2PConfig.PrivatePeerIDs = []string{"nope"}

	_, err := DefaultNewNode(config, log.TestingLogger())
	require.ErrorContains(t, err, "invalid p2p.libp2p.private_peer_ids entry")

	conn, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func TestNodeRegistersLibP2PPEX(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			config := newNodeTestConfig(t, "node_libp2p_pex_test")
			t.Cleanup(func() { _ = os.RemoveAll(config.RootDir) })
			config.P2P.ListenAddress = fmt.Sprintf("127.0.0.1:%d", utils.GetFreePorts(t, 1)[0])
			config.P2P.LibP2PConfig.Enabled = true
			config.P2P.LibP2PConfig.PEX.Enabled = enabled

			n, err := DefaultNewNode(config, log.TestingLogger())
			require.NoError(t, err)
			require.NoError(t, n.Start())
			t.Cleanup(func() { _ = n.Stop() })

			_, ok := n.sw.(*lp2p.Switch).Reactor("PEX")
			require.Equal(t, enabled, ok)
			require.Equal(t, enabled, slices.Contains(n.nodeInfo.(p2p.DefaultNodeInfo).Channels, lp2ppex.Channel))
		})
	}
}

func TestNodeDelayedStart(t *testing.T) {
	config := newNodeTestConfig(t, "node_delayed_start_test")
	defer os.RemoveAll(config.RootDir)
	now := cmttime.Now()

	// create & start node
	n, err := DefaultNewNode(config, log.TestingLogger())
	n.GenesisDoc().GenesisTime = now.Add(2 * time.Second)
	require.NoError(t, err)

	err = n.Start()
	require.NoError(t, err)
	defer n.Stop() //nolint:errcheck // ignore for tests

	startTime := cmttime.Now()
	assert.Equal(t, true, startTime.After(n.GenesisDoc().GenesisTime))
}

func TestNodeSetAppVersion(t *testing.T) {
	config := test.ResetTestRoot("node_app_version_test")
	defer os.RemoveAll(config.RootDir)

	// create & start node
	n, err := DefaultNewNode(config, log.TestingLogger())
	require.NoError(t, err)

	// default config uses the kvstore app
	appVersion := kvstore.AppVersion

	// check version is set in state
	state, err := n.stateStore.Load()
	require.NoError(t, err)
	assert.Equal(t, state.Version.Consensus.App, appVersion)

	// check version is set in node info
	assert.Equal(t, n.nodeInfo.(p2p.DefaultNodeInfo).ProtocolVersion.App, appVersion)
}

func TestPprofServer(t *testing.T) {
	config := newNodeTestConfig(t, "node_pprof_test")
	defer os.RemoveAll(config.RootDir)
	config.RPC.PprofListenAddress = "127.0.0.1:0"

	n, err := DefaultNewNode(config, log.TestingLogger())
	assert.NoError(t, err)
	assert.NoError(t, n.Start())
	defer func() {
		require.NoError(t, n.Stop())
	}()
	require.NotNil(t, n.pprofSrv)
	require.NotNil(t, n.pprofLn)

	pprofAddr := n.pprofLn.Addr().String()
	resp, err := http.Get("http://" + pprofAddr + "/debug/pprof")
	assert.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)
}

func TestNodeSetPrivValTCP(t *testing.T) {
	addr := "tcp://" + testFreeAddr(t)

	config := test.ResetTestRoot("node_priv_val_tcp_test")
	defer os.RemoveAll(config.RootDir)
	config.PrivValidatorListenAddr = addr

	dialer := privval.DialTCPFn(addr, 100*time.Millisecond, ed25519.GenPrivKey())
	dialerEndpoint := privval.NewSignerDialerEndpoint(
		log.TestingLogger(),
		dialer,
	)
	privval.SignerDialerEndpointTimeoutReadWrite(100 * time.Millisecond)(dialerEndpoint)

	signerServer := privval.NewSignerServer(
		dialerEndpoint,
		test.DefaultTestChainID,
		types.NewMockPV(),
	)

	go func() {
		err := signerServer.Start()
		if err != nil {
			panic(err)
		}
	}()
	defer signerServer.Stop() //nolint:errcheck // ignore for tests

	n, err := DefaultNewNode(config, log.TestingLogger())
	require.NoError(t, err)
	assert.IsType(t, &privval.RetrySignerClient{}, n.PrivValidator())
}

// address without a protocol must result in error
func TestPrivValidatorListenAddrNoProtocol(t *testing.T) {
	addrNoPrefix := testFreeAddr(t)

	config := test.ResetTestRoot("node_priv_val_tcp_test")
	defer os.RemoveAll(config.RootDir)
	config.PrivValidatorListenAddr = addrNoPrefix

	_, err := DefaultNewNode(config, log.TestingLogger())
	assert.Error(t, err)
}

func TestNodeSetPrivValIPC(t *testing.T) {
	tmpfile := "/tmp/kms." + cmtrand.Str(6) + ".sock"
	defer os.Remove(tmpfile) // clean up

	config := test.ResetTestRoot("node_priv_val_tcp_test")
	defer os.RemoveAll(config.RootDir)
	config.PrivValidatorListenAddr = "unix://" + tmpfile

	dialer := privval.DialUnixFn(tmpfile)
	dialerEndpoint := privval.NewSignerDialerEndpoint(
		log.TestingLogger(),
		dialer,
	)
	privval.SignerDialerEndpointTimeoutReadWrite(100 * time.Millisecond)(dialerEndpoint)

	pvsc := privval.NewSignerServer(
		dialerEndpoint,
		test.DefaultTestChainID,
		types.NewMockPV(),
	)

	go func() {
		err := pvsc.Start()
		require.NoError(t, err)
	}()
	defer pvsc.Stop() //nolint:errcheck // ignore for tests

	n, err := DefaultNewNode(config, log.TestingLogger())
	require.NoError(t, err)
	assert.IsType(t, &privval.RetrySignerClient{}, n.PrivValidator())
}

// testFreeAddr claims a free port so we don't block on listener being ready.
func testFreeAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	return fmt.Sprintf("127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port)
}

// TestStartRPCCleansUpOnFailure verifies that startRPC closes any listeners it
// opened when a later listen call fails.
func TestStartRPCCleansUpOnFailure(t *testing.T) {
	config := test.ResetTestRoot("node_startrpc_cleanup_test")
	defer os.RemoveAll(config.RootDir)
	config.RPC.GRPCListenAddress = ""

	// Bind both before releasing l1 so the OS can't reuse l1's port for l2.
	l1, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	l2, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l2.Close()
	firstAddr := l1.Addr().String()
	l1.Close()

	config.RPC.ListenAddress = fmt.Sprintf("tcp://%s,tcp://%s", firstAddr, l2.Addr().String())

	n, err := DefaultNewNode(config, log.TestingLogger())
	require.NoError(t, err)

	_, err = n.startRPC()
	require.Error(t, err)

	probe, err := net.Listen("tcp", firstAddr)
	require.NoError(t, err, "startRPC leaked first listener on %s", firstAddr)
	probe.Close()
}

// newNodeTestConfig returns a test config with kernel-assigned ephemeral P2P
// and RPC ports to avoid cross-test port conflicts.
func newNodeTestConfig(t *testing.T, testName string) *cfg.Config {
	t.Helper()
	config := test.ResetTestRoot(testName)
	config.P2P.ListenAddress = "tcp://127.0.0.1:0"
	config.RPC.ListenAddress = "tcp://127.0.0.1:0"
	config.RPC.GRPCListenAddress = ""
	return config
}

// create a proposal block using real and full
// mempool and evidence pool and validate it.
func TestCreateProposalBlock(t *testing.T) {
	ctx := t.Context()

	config := test.ResetTestRoot("node_create_proposal")
	defer os.RemoveAll(config.RootDir)
	cc := proxy.NewLocalClientCreator(kvstore.NewInMemoryApplication())
	proxyApp := proxy.NewAppConns(cc, proxy.NopMetrics())
	err := proxyApp.Start()
	require.Nil(t, err)
	defer proxyApp.Stop() //nolint:errcheck // ignore for tests

	logger := log.TestingLogger()

	var height int64 = 1
	state, stateDB, privVals := state(1, height)
	stateStore := sm.NewStore(stateDB, sm.StoreOptions{
		DiscardABCIResponses: false,
	})
	var (
		partSize uint32 = 256
		maxBytes int64  = 16384
	)
	maxEvidenceBytes := maxBytes / 2
	state.ConsensusParams.Block.MaxBytes = maxBytes
	state.ConsensusParams.Evidence.MaxBytes = maxEvidenceBytes
	proposerAddr, _ := state.Validators.GetByIndex(0)

	// Make Mempool
	memplMetrics := mempl.NopMetrics()
	mempool := mempl.NewCListMempool(config.Mempool,
		proxyApp.Mempool(),
		state.LastBlockHeight,
		mempl.WithMetrics(memplMetrics),
		mempl.WithPreCheck(sm.TxPreCheck(state)),
		mempl.WithPostCheck(sm.TxPostCheck(state)))

	// Make EvidencePool
	evidenceDB := dbm.NewMemDB()
	blockStore := store.NewBlockStore(dbm.NewMemDB())
	evidencePool, err := evidence.NewPool(evidenceDB, stateStore, blockStore)
	require.NoError(t, err)
	evidencePool.SetLogger(logger)

	// fill the evidence pool with more evidence
	// than can fit in a block
	var currentBytes int64
	for currentBytes <= maxEvidenceBytes {
		ev, err := types.NewMockDuplicateVoteEvidenceWithValidator(height, time.Now(), privVals[0], "test-chain")
		require.NoError(t, err)
		currentBytes += int64(len(ev.Bytes()))
		evidencePool.ReportConflictingVotes(ev.VoteA, ev.VoteB)
	}

	evList, size := evidencePool.PendingEvidence(state.ConsensusParams.Evidence.MaxBytes)
	require.Less(t, size, state.ConsensusParams.Evidence.MaxBytes+1)
	evData := &types.EvidenceData{Evidence: evList}
	require.EqualValues(t, size, evData.ByteSize())

	// fill the mempool with more txs
	// than can fit in a block
	txLength := 100
	for i := 0; i <= int(maxBytes)/txLength; i++ {
		tx := cmtrand.Bytes(txLength)
		err := mempool.CheckTx(tx, nil, mempl.TxInfo{})
		assert.NoError(t, err)
	}

	blockExec := sm.NewBlockExecutor(
		stateStore,
		logger,
		proxyApp.Consensus(),
		mempool,
		evidencePool,
		blockStore,
	)

	extCommit := &types.ExtendedCommit{Height: height - 1}
	block, err := blockExec.CreateProposalBlock(
		ctx,
		height,
		state,
		extCommit,
		proposerAddr,
	)
	require.NoError(t, err)

	// check that the part set does not exceed the maximum block size
	partSet, err := block.MakePartSet(partSize)
	require.NoError(t, err)
	assert.Less(t, partSet.ByteSize(), maxBytes)

	partSetFromHeader := types.NewPartSetFromHeader(partSet.Header())
	for partSetFromHeader.Count() < partSetFromHeader.Total() {
		added, err := partSetFromHeader.AddPart(partSet.GetPart(int(partSetFromHeader.Count())))
		require.NoError(t, err)
		require.True(t, added)
	}
	assert.EqualValues(t, partSetFromHeader.ByteSize(), partSet.ByteSize())

	err = blockExec.ValidateBlock(state, block)
	assert.NoError(t, err)
}

func TestMaxProposalBlockSize(t *testing.T) {
	ctx := t.Context()

	config := test.ResetTestRoot("node_create_proposal")
	defer os.RemoveAll(config.RootDir)
	cc := proxy.NewLocalClientCreator(kvstore.NewInMemoryApplication())
	proxyApp := proxy.NewAppConns(cc, proxy.NopMetrics())
	err := proxyApp.Start()
	require.Nil(t, err)
	defer proxyApp.Stop() //nolint:errcheck // ignore for tests

	logger := log.TestingLogger()

	var height int64 = 1
	state, stateDB, _ := state(1, height)
	stateStore := sm.NewStore(stateDB, sm.StoreOptions{
		DiscardABCIResponses: false,
	})
	var maxBytes int64 = 16384
	var partSize uint32 = 256
	state.ConsensusParams.Block.MaxBytes = maxBytes
	proposerAddr, _ := state.Validators.GetByIndex(0)

	// Make Mempool
	memplMetrics := mempl.NopMetrics()
	mempool := mempl.NewCListMempool(config.Mempool,
		proxyApp.Mempool(),
		state.LastBlockHeight,
		mempl.WithMetrics(memplMetrics),
		mempl.WithPreCheck(sm.TxPreCheck(state)),
		mempl.WithPostCheck(sm.TxPostCheck(state)))

	blockStore := store.NewBlockStore(dbm.NewMemDB())

	// fill the mempool with one txs just below the maximum size
	txLength := int(types.MaxDataBytesNoEvidence(maxBytes, 1))
	tx := cmtrand.Bytes(txLength - 4) // to account for the varint
	err = mempool.CheckTx(tx, nil, mempl.TxInfo{})
	assert.NoError(t, err)

	blockExec := sm.NewBlockExecutor(
		stateStore,
		logger,
		proxyApp.Consensus(),
		mempool,
		sm.EmptyEvidencePool{},
		blockStore,
	)

	extCommit := &types.ExtendedCommit{Height: height - 1}
	block, err := blockExec.CreateProposalBlock(
		ctx,
		height,
		state,
		extCommit,
		proposerAddr,
	)
	require.NoError(t, err)

	pb, err := block.ToProto()
	require.NoError(t, err)
	assert.Less(t, int64(pb.Size()), maxBytes)

	// check that the part set does not exceed the maximum block size
	partSet, err := block.MakePartSet(partSize)
	require.NoError(t, err)
	assert.EqualValues(t, partSet.ByteSize(), int64(pb.Size()))
}

func TestNodeNewNodeCustomReactors(t *testing.T) {
	config := newNodeTestConfig(t, "node_new_node_custom_reactors_test")
	defer os.RemoveAll(config.RootDir)

	cr := p2pmock.NewReactor()
	cr.Channels = []*conn.ChannelDescriptor{
		{
			ID:                  byte(0x31),
			Priority:            5,
			SendQueueCapacity:   100,
			RecvMessageCapacity: 100,
		},
	}
	customBlocksyncReactor := p2pmock.NewReactor()

	nodeKey, err := p2p.LoadOrGenNodeKey(config.NodeKeyFile())
	require.NoError(t, err)

	n, err := NewNode(config,
		privval.LoadOrGenFilePV(config.PrivValidatorKeyFile(), config.PrivValidatorStateFile()),
		nodeKey,
		proxy.DefaultClientCreator(config.ProxyApp, config.ABCI, config.DBDir()),
		DefaultGenesisDocProviderFunc(config),
		cfg.DefaultDBProvider,
		DefaultMetricsProvider(config.Instrumentation),
		log.TestingLogger(),
		CustomReactors(map[string]p2p.Reactor{"FOO": cr, "BLOCKSYNC": customBlocksyncReactor}),
	)
	require.NoError(t, err)

	err = n.Start()
	require.NoError(t, err)
	defer n.Stop() //nolint:errcheck // ignore for tests

	assert.True(t, cr.IsRunning())
	reactor, ok := n.Switch().Reactor("FOO")
	assert.True(t, ok)
	assert.Equal(t, cr, reactor)

	assert.True(t, customBlocksyncReactor.IsRunning())

	reactor, ok = n.Switch().Reactor("BLOCKSYNC")
	assert.True(t, ok)
	assert.Equal(t, customBlocksyncReactor, reactor)

	channels := n.NodeInfo().(p2p.DefaultNodeInfo).Channels
	assert.Contains(t, channels, mempl.MempoolChannel)
	assert.Contains(t, channels, cr.Channels[0].ID)
}

func state(nVals int, height int64) (sm.State, dbm.DB, []types.PrivValidator) {
	privVals := make([]types.PrivValidator, nVals)
	vals := make([]types.GenesisValidator, nVals)
	for i := 0; i < nVals; i++ {
		privVal := types.NewMockPV()
		privVals[i] = privVal
		vals[i] = types.GenesisValidator{
			Address: privVal.PrivKey.PubKey().Address(),
			PubKey:  privVal.PrivKey.PubKey(),
			Power:   1000,
			Name:    fmt.Sprintf("test%d", i),
		}
	}
	s, _ := sm.MakeGenesisState(&types.GenesisDoc{
		ChainID:    "test-chain",
		Validators: vals,
		AppHash:    nil,
	})

	// save validators to db for 2 heights
	stateDB := dbm.NewMemDB()
	stateStore := sm.NewStore(stateDB, sm.StoreOptions{
		DiscardABCIResponses: false,
	})
	if err := stateStore.Save(s); err != nil {
		panic(err)
	}

	for i := 1; i < int(height); i++ {
		s.LastBlockHeight++
		s.LastValidators = s.Validators.Copy()
		if err := stateStore.Save(s); err != nil {
			panic(err)
		}
	}
	return s, stateDB, privVals
}

func randomPeerID(t *testing.T) peer.ID {
	t.Helper()

	id, err := lp2p.IDFromPrivateKey(ed25519.GenPrivKey())
	require.NoError(t, err)

	return id
}

type peersSwitch struct {
	p2p.Switcher
	peers p2p.IPeerSet
}

func (s *peersSwitch) Peers() p2p.IPeerSet { return s.peers }

type idPeer struct {
	*p2pmock.Peer
	id p2p.ID
}

func (p *idPeer) ID() p2p.ID { return p.id }
