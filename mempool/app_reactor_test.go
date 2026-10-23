package mempool

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	abcimock "github.com/cometbft/cometbft/abci/client/mocks"
	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/internal/protowire"
	"github.com/cometbft/cometbft/libs/log"
	"github.com/cometbft/cometbft/libs/rand"
	"github.com/cometbft/cometbft/p2p"
	p2pmock "github.com/cometbft/cometbft/p2p/mock"
	protomem "github.com/cometbft/cometbft/proto/tendermint/mempool"
	"github.com/cometbft/cometbft/types"
	"github.com/fortytw2/leaktest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func insertTxReq(tx string) any {
	return mock.MatchedBy(func(req *abci.RequestInsertTx) bool { return string(req.Tx) == tx })
}

func receiveTxs(r *AppReactor, txs ...string) {
	msg := &protomem.Txs{}
	for _, tx := range txs {
		msg.Txs = append(msg.Txs, []byte(tx))
	}
	r.Receive(p2p.Envelope{Src: p2pmock.NewPeer(net.IP{127, 0, 0, 1}), ChannelID: MempoolChannel, Message: msg})
}

func TestAppReactor(t *testing.T) {
	t.Run("effectiveMaxBatchBytes", func(t *testing.T) {
		for _, tt := range []struct {
			name          string
			maxTxBytes    int
			maxBatchBytes int
			want          int
		}{
			{"MaxBatchBytes unset falls back to MaxTxBytes", 1000, 0, 1000},
			{"MaxBatchBytes set and larger than MaxTxBytes", 1000, 5000, 5000},
			{"MaxBatchBytes set and smaller than MaxTxBytes", 1000, 500, 500},
		} {
			t.Run(tt.name, func(t *testing.T) {
				r := &AppReactor{config: &config.MempoolConfig{
					MaxTxBytes:    tt.maxTxBytes,
					MaxBatchBytes: tt.maxBatchBytes,
				}}
				require.Equal(t, tt.want, r.effectiveMaxBatchBytes())
			})
		}
	})

	// Regression test for the peer-teardown bug: every batch produced by chunkTxs
	// must serialize to a Message that fits within RecvMessageCapacity.
	t.Run("GetChannels", func(t *testing.T) {
		for _, tt := range []struct {
			name          string
			maxTxBytes    int
			maxBatchBytes int
			txSize        int
			txCount       int
		}{
			// Many 1-byte txs: worst-case proto framing ratio (2 overhead bytes per 1 data byte).
			{"1-byte txs default batch limit", 10000, 0, 1, 10000},
			// Txs at the 127-byte boundary (varint transitions from 1 to 2 bytes at 128).
			{"127-byte txs default batch limit", 10000, 0, 127, 100},
			{"128-byte txs default batch limit", 10000, 0, 128, 100},
			// Large txs near MaxTxBytes.
			{"large txs default batch limit", 10000, 0, 9000, 5},
			// MaxBatchBytes set larger than MaxTxBytes.
			{"small txs explicit batch limit", 1000, 5000, 1, 5000},
		} {
			t.Run(tt.name, func(t *testing.T) {
				r := &AppReactor{config: &config.MempoolConfig{
					MaxTxBytes:    tt.maxTxBytes,
					MaxBatchBytes: tt.maxBatchBytes,
				}}

				txs := make(types.Txs, tt.txCount)
				for i := range txs {
					txs[i] = make(types.Tx, tt.txSize)
				}

				recvCap := r.GetChannels()[0].RecvMessageCapacity
				batches := chunkTxs(txs, r.effectiveMaxBatchBytes())

				for i, batch := range batches {
					msg := &protomem.Message{
						Sum: &protomem.Message_Txs{
							Txs: &protomem.Txs{Txs: batch.ToSliceOfBytes()},
						},
					}
					msgSize := msg.Size()
					require.LessOrEqual(t, msgSize, recvCap,
						"batch %d serialized size %d exceeds RecvMessageCapacity %d", i, msgSize, recvCap)
				}
			})
		}
	})

	t.Run("Receive keeps inserting after a panicking tx", func(t *testing.T) {
		app := abcimock.NewClient(t)
		app.On("InsertTx", mock.Anything, insertTxReq("poison-1")).Return(insertPanics)
		app.On("InsertTx", mock.Anything, insertTxReq("poison-2")).Return(insertPanics)
		app.On("InsertTx", mock.Anything, insertTxReq("good")).
			Return(&abci.ResponseInsertTx{Code: abci.CodeTypeOK}, nil).Once()

		cfg := config.TestConfig().Mempool
		r := NewAppReactor(cfg, newTestAppMempool(t, cfg, app), false)

		var logs lockedBuffer
		r.SetLogger(log.NewTMLogger(&logs))

		require.NotPanics(t, func() { receiveTxs(r, "poison-1", "poison-2", "good") })

		out := logs.String()
		requireFirstAtRestDebug(t, out, "Panic inserting tx", "E[")
		require.Equal(t, 1, strings.Count(out, "stack="), out)
	})

	t.Run("Receive logs insert failures at Info once per interval and the rest at Debug", func(t *testing.T) {
		app := abcimock.NewClient(t)
		app.On("InsertTx", mock.Anything, mock.Anything).Return(&abci.ResponseInsertTx{Code: 123}, nil)

		cfg := config.TestConfig().Mempool
		m := newTestAppMempool(t, cfg, app)
		r := NewAppReactor(cfg, m, false)

		var logs lockedBuffer
		r.SetLogger(log.NewTMLogger(&logs))
		receiveTxs(r, "rejected-1", "rejected-2")

		out := logs.String()
		requireFirstAtRestDebug(t, out, "Failed to insert tx", "I[")
		require.True(t, m.checkTxErrorLogs.claim(), "insert failures must not use the CheckTx error log budget")
		require.True(t, m.txPanicStacks.claim(), "insert failures must not use the per-tx panic stack budget")
		require.True(t, m.panicStacks.claim(), "insert failures must not use the one-off stack budget")
	})

	t.Run("Receive logs an oversized tx at Debug", func(t *testing.T) {
		cfg := config.TestConfig().Mempool
		cfg.MaxTxBytes = 4
		r := NewAppReactor(cfg, newTestAppMempool(t, cfg, abcimock.NewClient(t)), false)

		var logs lockedBuffer
		r.SetLogger(log.NewTMLogger(&logs))
		receiveTxs(r, "larger-than-four")

		out := logs.String()
		require.NotContains(t, out, "Failed to insert tx", out)

		if log.LogDebug {
			lines := logLinesWith(out, "Tx too large")
			require.Len(t, lines, 1, out)
			require.True(t, strings.HasPrefix(lines[0], "D["), out)
		}
	})

	t.Run("reactor shares its mempool's per-tx panic-stack budget", func(t *testing.T) {
		app := abcimock.NewClient(t)
		app.On("InsertTx", mock.Anything, mock.Anything).
			Return(insertPanics)

		cfg := config.TestConfig().Mempool
		m := newTestAppMempool(t, cfg, app)
		r := NewAppReactor(cfg, m, false)
		require.True(t, m.txPanicStacks.claim(), "a CheckTx panic takes this minute's stack")

		var logs lockedBuffer
		r.SetLogger(log.NewTMLogger(&logs))
		receiveTxs(r, "poison")

		out := logs.String()
		require.NotContains(t, out, "stack=", out)
	})

	t.Run("broadcast routine logs a panic at Error", func(t *testing.T) {
		cfg := config.TestConfig().Mempool
		cfg.Broadcast = true
		cfg.ReapInterval = 5 * time.Millisecond

		app := abcimock.NewClient(t)
		app.On("ReapTxs", mock.Anything, mock.Anything).Return(&abci.ResponseReapTxs{Txs: [][]byte{[]byte("to-broadcast")}}, nil).Maybe()

		metrics := NopMetrics()
		metrics.BatchSize = panickingHistogram{}
		m := newTestAppMempool(t, cfg, app, WithAMMetrics(metrics))
		r := NewAppReactor(cfg, m, false)
		require.True(t, m.panicStacks.claim())

		var logs lockedBuffer
		r.SetLogger(log.NewTMLogger(&logs))

		require.NoError(t, r.Start())
		t.Cleanup(func() { _ = r.Stop() })

		require.Eventually(t, func() bool {
			return strings.Contains(logs.String(), "Panic in broadcast routine")
		}, 2*time.Second, 10*time.Millisecond)
		require.NoError(t, r.Stop())

		requireOneErrorWithoutStack(t, logs.String(), "Panic in broadcast routine")
	})

	t.Run("OnStop closes mempool", func(t *testing.T) {
		checkLeaks := leaktest.CheckTimeout(t, 5*time.Second)

		cfg := config.TestConfig().Mempool
		m := NewAppMempool(cfg, abcimock.NewClient(t))
		r := NewAppReactor(cfg, m, true)

		require.NoError(t, r.Start())
		require.NoError(t, r.Stop())
		checkLeaks()

		require.NotPanics(t, m.Close)
	})

	const (
		timeout  = 5 * time.Second
		interval = 200 * time.Millisecond
	)

	eventually := func(fn func() bool) {
		require.Eventually(t, fn, timeout, interval)
	}

	// ARRANGE
	// Given 3 nodes
	var (
		nodeA = newAppReactorNode(t, "A")
		nodeB = newAppReactorNode(t, "B")
		nodeC = newAppReactorNode(t, "C")
		nodes = []*appReactorNode{nodeA, nodeB, nodeC}
	)

	// With switches connected & mempool initialized
	onStart := func(i int, s *p2p.Switch) *p2p.Switch {
		s.AddReactor("MEMPOOL", nodes[i].reactor)
		return s
	}

	switches := p2p.MakeConnectedSwitches(config.TestConfig().P2P, len(nodes), onStart, p2p.Connect2Switches)

	for i, node := range nodes {
		node.sw = switches[i]
	}

	defer func() {
		for _, node := range nodes {
			if err := node.sw.Stop(); err != nil {
				require.NoError(t, err)
			}
		}
	}()

	// ACT #0
	// Enable B and C but keep A gated on waitForSync.
	// A tx inserted into A must not reach peers until A is enabled.
	nodeB.reactor.EnableInOutTxs()
	nodeC.reactor.EnableInOutTxs()

	gatedTx := types.Tx("gated_until_sync")
	require.NoError(t, nodeA.mempool.InsertTx(gatedTx))

	// ASSERT #0
	// Broadcast from A stays blocked — B and C must not receive gatedTx.
	require.Never(t, func() bool {
		return txsContain(nodeB.getReceivedTxs(), types.Txs{gatedTx}) ||
			txsContain(nodeC.getReceivedTxs(), types.Txs{gatedTx})
	}, 500*time.Millisecond, 100*time.Millisecond)

	// Enable A; gatedTx should now propagate.
	nodeA.reactor.EnableInOutTxs()
	eventually(func() bool {
		return txsContain(nodeB.getReceivedTxs(), types.Txs{gatedTx}) &&
			txsContain(nodeC.getReceivedTxs(), types.Txs{gatedTx})
	})

	// ACT #1
	// Insert several txs into A
	txs1 := []types.Tx{
		types.Tx("from_a_to_b:1"),
		types.Tx("from_a_to_b:2"),
		types.Tx("from_a_to_b:3"),
		types.Tx("from_a_to_b:4"),
		types.Tx("from_a_to_b:5"),
	}
	for _, tx := range txs1 {
		err := nodeA.mempool.InsertTx(tx)
		require.NoError(t, err, "failed to insert tx %q into node A", tx)
	}

	// ASSERT #1
	// Wait for txs to arrive at B
	eventually(func() bool {
		received := nodeB.getReceivedTxs()
		return txsContain(received, txs1)
	})

	// ACT #2
	// Insert several txs into B
	txs2 := []types.Tx{
		types.Tx("from_b_to_a:1"),
		types.Tx("from_b_to_a:2"),
		types.Tx("from_b_to_a:3"),
		types.Tx("from_b_to_a:4"),
		types.Tx("from_b_to_a:5"),
	}
	for _, tx := range txs2 {
		err := nodeB.mempool.InsertTx(tx)
		require.NoError(t, err, "failed to insert tx %q into node B", tx)
	}

	// ASSERT #2
	// Wait for txs to arrive at A
	eventually(func() bool {
		received := nodeA.getReceivedTxs()
		return txsContain(received, txs2)
	})

	// ASSERT #3
	// Ensure all nodes (including C) have all txs
	//nolint:gocritic
	allTxs := append(txs1, txs2...)
	eventually(func() bool {
		receivedA := nodeA.getReceivedTxs()
		receivedB := nodeB.getReceivedTxs()
		receivedC := nodeC.getReceivedTxs()
		return txsContain(receivedA, allTxs) &&
			txsContain(receivedB, allTxs) &&
			txsContain(receivedC, allTxs)
	})

	// Also make sure that all txs are unique in each node
	require.False(t, hasDuplicates(nodeA.getReceivedTxs()))
	require.False(t, hasDuplicates(nodeB.getReceivedTxs()))
	require.False(t, hasDuplicates(nodeC.getReceivedTxs()))
}

func TestChunkTxs(t *testing.T) {
	makeTx := func(size int) types.Tx {
		return types.Tx(rand.Bytes(size))
	}

	toTxs := func(sizes []int) types.Txs {
		txs := make([]types.Tx, 0, len(sizes))
		for _, size := range sizes {
			txs = append(txs, makeTx(size))
		}
		return txs
	}

	for _, tt := range []struct {
		name   string
		input  []int
		size   int
		output [][]int
	}{
		{
			name:   "single tx smaller than size",
			input:  []int{100},
			size:   200,
			output: [][]int{{100}},
		},
		{
			name:   "single tx bigger than size",
			input:  []int{100},
			size:   50,
			output: [][]int{{100}},
		},
		{
			// Two 100-byte txs each cost 102 effective bytes (2 bytes proto framing).
			// 204 fits exactly two; a third would overflow.
			name:   "basic",
			input:  []int{100, 100, 100},
			size:   204,
			output: [][]int{{100, 100}, {100}},
		},
		{
			name:   "txs equal size",
			input:  []int{100, 100, 100},
			size:   100,
			output: [][]int{{100}, {100}, {100}},
		},
		{
			// With 2-byte proto framing per tx, [20,30] sums to 54 effective bytes;
			// adding 50 would reach 106 > 100, so [50] starts a new chunk with [2].
			name:   "edge-case",
			input:  []int{101, 20, 30, 50, 2, 102, 3},
			size:   100,
			output: [][]int{{101}, {20, 30}, {50, 2}, {102}, {3}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// ARRANGE
			input := toTxs(tt.input)

			expected := make([]types.Txs, 0, len(tt.output))
			for _, chunk := range tt.output {
				expected = append(expected, toTxs(chunk))
			}

			// ACT
			actual := chunkTxs(input, tt.size)

			// ASSERT
			require.Equal(t, len(expected), len(actual), "output length mismatch")

			for i, chunk := range actual {
				require.Equal(t, len(expected[i]), len(chunk), "chunk length mismatch (#%d)", i)

				// Verify each multi-tx chunk's effective proto size stays within the limit.
				// Single-tx chunks are exempt: an oversized lone tx must still be forwarded.
				if len(chunk) <= 1 {
					continue
				}
				effectiveSize := 0
				for _, tx := range chunk {
					effectiveSize += len(tx) + protowire.RepeatedBytesEntrySize(len(tx))
				}
				require.LessOrEqual(t, effectiveSize, tt.size,
					"chunk #%d effective size %d exceeds limit %d", i, effectiveSize, tt.size)
			}
		})
	}
}

type appReactorNode struct {
	t    *testing.T
	name string

	app     *abcimock.Client
	mempool *AppMempool
	reactor *AppReactor
	sw      *p2p.Switch

	mempoolTxs  types.Txs
	receivedTxs types.Txs
	mu          sync.Mutex

	logger log.Logger
}

func newAppReactorNode(t *testing.T, name string) *appReactorNode {
	config := config.TestConfig()
	logger := log.TestingLogger().With("name", name)
	app := abcimock.NewClient(t)

	mempool := NewAppMempool(
		config.Mempool,
		app,
		WithAMLogger(logger.With("module", "mempool")),
	)

	reactor := NewAppReactor(config.Mempool, mempool, true)
	reactor.SetLogger(logger.With("module", "reactor"))

	ts := &appReactorNode{
		t:       t,
		name:    name,
		app:     app,
		mempool: mempool,
		reactor: reactor,
		logger:  logger,
	}

	ts.setupAppMock()

	return ts
}

func (ts *appReactorNode) insertTx(tx types.Tx) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	ts.logger.Info("inserting tx", "tx", string(tx))

	ts.mempoolTxs = append(ts.mempoolTxs, tx)
	ts.receivedTxs = append(ts.receivedTxs, tx)
}

func (ts *appReactorNode) reapTxs() types.Txs {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	ts.logger.Info("reaping txs")

	out := make(types.Txs, 0, len(ts.mempoolTxs))
	out = append(out, ts.mempoolTxs...)

	ts.mempoolTxs = ts.mempoolTxs[:0]

	return out
}

func (ts *appReactorNode) getReceivedTxs() types.Txs {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	out := make(types.Txs, 0, len(ts.receivedTxs))
	out = append(out, ts.receivedTxs...)

	return out
}

func (ts *appReactorNode) setupAppMock() {
	mockGrpc := func(method string, fn any) *mock.Call {
		return ts.app.On(method, mock.Anything, mock.Anything).Return(fn).Maybe()
	}

	mockGrpc("InsertTx", func(_ context.Context, req *abci.RequestInsertTx) (*abci.ResponseInsertTx, error) {
		ts.insertTx(req.Tx)
		return &abci.ResponseInsertTx{
			Code: abci.CodeTypeOK,
		}, nil
	})

	mockGrpc("ReapTxs", func(_ context.Context, req *abci.RequestReapTxs) (*abci.ResponseReapTxs, error) {
		out := ts.reapTxs()

		return &abci.ResponseReapTxs{Txs: out.ToSliceOfBytes()}, nil
	})
}

func txsContain(set, subset types.Txs) bool {
	cache := make(map[string]struct{})

	for _, tx := range set {
		cache[tx.String()] = struct{}{}
	}

	for _, tx := range subset {
		if _, ok := cache[tx.String()]; !ok {
			return false
		}
	}

	return true
}

func hasDuplicates(txs types.Txs) bool {
	cache := make(map[string]struct{})

	for _, tx := range txs {
		if _, ok := cache[tx.String()]; ok {
			return true
		}

		cache[tx.String()] = struct{}{}
	}

	return false
}
