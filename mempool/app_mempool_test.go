package mempool

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	abcimock "github.com/cometbft/cometbft/abci/client/mocks"
	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/libs/log"
	"github.com/cometbft/cometbft/types"
	kitmetrics "github.com/go-kit/kit/metrics"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func newTestAppMempool(t *testing.T, cfg *config.MempoolConfig, app AppMempoolClient, opts ...AppMempoolOpt) *AppMempool {
	t.Helper()
	m := NewAppMempool(cfg, app, opts...)
	t.Cleanup(m.Close)
	return m
}

func checkTxResult(t *testing.T, m *AppMempool, tx types.Tx) *abci.ResponseCheckTx {
	t.Helper()
	resCh := make(chan *abci.ResponseCheckTx, 1)
	require.NoError(t, m.CheckTx(tx, func(res *abci.ResponseCheckTx) { resCh <- res }, TxInfo{}))
	select {
	case res := <-resCh:
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("CheckTx callback not called")
		return nil
	}
}

func TestAppMempool(t *testing.T) {
	tx := func(v string) types.Tx { return types.Tx(v) }

	const codeFail = uint32(123)

	t.Run("InsertTx", func(t *testing.T) {
		// ARRANGE
		wasInserted := atomic.Uint64{}

		// Given app
		app := abcimock.NewClient(t)
		app.
			On("InsertTx", mock.Anything, mock.Anything).
			Return(func(_ context.Context, req *abci.RequestInsertTx) (*abci.ResponseInsertTx, error) {
				if string(req.Tx) == "fail-retryable" {
					return &abci.ResponseInsertTx{Code: abci.CodeTypeRetry}, nil
				}
				if string(req.Tx) == "fail" {
					return &abci.ResponseInsertTx{Code: codeFail}, nil
				}

				wasInserted.Add(1)
				return &abci.ResponseInsertTx{Code: abci.CodeTypeOK}, nil
			})
		app.
			On("CheckTx", mock.Anything, mock.Anything).
			Return(func(_ context.Context, req *abci.RequestCheckTx) (*abci.ResponseCheckTx, error) {
				if string(req.Tx) == "fail-retryable" {
					return &abci.ResponseCheckTx{Code: abci.CodeTypeRetry}, nil
				}
				if string(req.Tx) == "fail" {
					return &abci.ResponseCheckTx{Code: codeFail}, nil
				}
				return &abci.ResponseCheckTx{Code: abci.CodeTypeOK}, nil
			})

		// Given mempool
		m := newTestAppMempool(t, config.DefaultMempoolConfig(), app)

		// Given txs
		txs := []types.Tx{tx("tx1"), tx("tx2"), tx(""), tx("fail-retryable")}

		// ACT
		err1 := m.InsertTx(txs[0])
		err2 := m.InsertTx(txs[1])
		err3 := m.InsertTx(txs[0]) // seen tx
		err4 := m.InsertTx(txs[2]) // empty tx
		err5 := m.InsertTx(txs[3]) // retryable error

		// ASSERT
		require.NoError(t, err1)
		require.NoError(t, err2)

		require.ErrorIs(t, err3, ErrSeenTx)
		require.ErrorIs(t, err4, ErrEmptyTx)

		require.ErrorContains(t, err5, "invalid code: (code=32000)")
		require.Equal(t, uint64(2), wasInserted.Load())

		// retryable txs are forgotten asynchronously via guard.ForgetAfter(),
		// so the seen-cache entry clears shortly after the call returns.
		retryableForgotten := func() bool {
			return !m.guard.Has(txs[3].Key())
		}
		require.Eventually(t, retryableForgotten, m.checkTxRetryDelay, 50*time.Millisecond)

		t.Run("CheckTx", func(t *testing.T) {
			for _, tt := range []struct {
				name        string
				tx          types.Tx
				errContains string
				noCallback  bool
				assert      func(t *testing.T, res *abci.ResponseCheckTx)
			}{
				{
					name:        "seen",
					tx:          tx("tx1"),
					errContains: "already seen",
				},
				{
					name: "fail",
					tx:   tx("fail"),
					assert: func(t *testing.T, res *abci.ResponseCheckTx) {
						require.Equal(t, codeFail, res.Code)
					},
				},
				{
					name: "fail-retryable",
					tx:   tx("fail-retryable"),
					assert: func(t *testing.T, res *abci.ResponseCheckTx) {
						require.Equal(t, abci.CodeTypeRetry, res.Code)
						require.True(t, m.guard.Has(tx("fail-retryable").Key()))

						// eventually the tx should be forgotten
						check := func() bool {
							return !m.guard.Has(tx("fail-retryable").Key())
						}

						require.Eventually(t, check, m.checkTxRetryDelay, time.Millisecond*50)
					},
				},
				{
					name: "ok",
					tx:   tx("ok"),
					assert: func(t *testing.T, res *abci.ResponseCheckTx) {
						require.Equal(t, abci.CodeTypeOK, res.Code)
					},
				},
				{
					name: "ok-no-callback",
					tx:   tx("ok2"),
				},
			} {
				t.Run(tt.name, func(t *testing.T) {
					// ARRANGE
					var (
						result       = atomic.Pointer[abci.ResponseCheckTx]{}
						callback     = func(res *abci.ResponseCheckTx) { result.Store(res) }
						ensureResult = func() bool { return result.Load() != nil }
					)

					if tt.noCallback {
						callback = nil
					}

					// ACT
					err := m.CheckTx(tt.tx, callback, TxInfo{})

					// ASSERT
					if tt.errContains != "" {
						require.ErrorContains(t, err, tt.errContains)
						return
					}

					require.NoError(t, err)
					require.Eventually(t, ensureResult, time.Second, time.Millisecond*50)

					if tt.assert != nil {
						tt.assert(t, result.Load())
					}
				})
			}
		})

		t.Run("forgetTx runs even if callback panics", func(t *testing.T) {
			cfg := config.TestMempoolConfig()
			cfg.CheckTxRetryDelay = 50 * time.Millisecond

			app := abcimock.NewClient(t)
			app.On("CheckTx", mock.Anything, mock.Anything).
				Return(&abci.ResponseCheckTx{Code: abci.CodeTypeRetry}, nil)

			var logs lockedBuffer
			m := newTestAppMempool(t, cfg, app, WithAMLogger(log.NewTMLogger(&logs)))
			require.True(t, m.panicStacks.claim())
			tx := types.Tx("panic-tx")

			require.NoError(t, m.CheckTx(tx, func(_ *abci.ResponseCheckTx) { panic("callback panic") }, TxInfo{}))
			require.Eventually(t, func() bool {
				return !m.guard.Has(tx.Key())
			}, cfg.CheckTxRetryDelay, 5*time.Millisecond)

			require.Eventually(t, func() bool {
				return strings.Contains(logs.String(), "panic in the AppMempool CheckTx goroutine")
			}, time.Second, 5*time.Millisecond)
			requireOneErrorWithoutStack(t, logs.String(), "panic in the AppMempool CheckTx goroutine")
		})

		t.Run("app panics log one stack per interval at Error and the rest at Debug", func(t *testing.T) {
			app := abcimock.NewClient(t)
			app.On("CheckTx", mock.Anything, mock.Anything).
				Return(checkTxPanics)

			var logs lockedBuffer
			m := newTestAppMempool(t, config.TestMempoolConfig(), app, WithAMLogger(log.NewTMLogger(&logs)))

			for _, panicTx := range []types.Tx{tx("panic-1"), tx("panic-2")} {
				res := checkTxResult(t, m, panicTx)
				require.Equal(t, abci.CodeTypeInternal, res.Code)
			}

			out := logs.String()
			require.Equal(t, 1, strings.Count(out, "stack="), out)
			lines := requireFirstAtRestDebug(t, out, "panic in AppMempool.CheckTx", "E[")
			require.Contains(t, lines[0], "stack=", out)
			require.False(t, m.txPanicStacks.claim(), "CheckTx panics use the per-tx stack budget the reactor shares")
		})

		t.Run("app errors log at Error once per interval and the rest at Debug", func(t *testing.T) {
			app := abcimock.NewClient(t)
			app.On("CheckTx", mock.Anything, mock.Anything).Return((*abci.ResponseCheckTx)(nil), fmt.Errorf("connection error"))

			var logs lockedBuffer
			m := newTestAppMempool(t, config.TestMempoolConfig(), app, WithAMLogger(log.NewTMLogger(&logs)))

			for _, failedTx := range []types.Tx{tx("error-1"), tx("error-2")} {
				require.Equal(t, abci.CodeTypeInternal, checkTxResult(t, m, failedTx).Code)
			}

			out := logs.String()
			requireFirstAtRestDebug(t, out, "error checking tx", "E[")
			require.True(t, m.txPanicStacks.claim(), "CheckTx app errors must not use the per-tx panic stack budget")
			require.True(t, m.panicStacks.claim(), "CheckTx app errors must not use the one-off stack budget")
		})
	})

	t.Run("TxStream", func(t *testing.T) {
		// ARRANGE
		const amount = 100
		const callsToCancel = 4

		// Given context
		ctx, cancel := context.WithCancel(context.Background())
		calls := atomic.Uint64{}

		// Given app
		allMempoolTxs := [][]byte{}

		app := abcimock.NewClient(t)
		app.
			On("ReapTxs", mock.Anything, mock.Anything).
			Return(func(_ context.Context, _ *abci.RequestReapTxs) (*abci.ResponseReapTxs, error) {
				txs := make([][]byte, 0, amount)
				for i := 0; i < amount; i++ {
					txs = append(txs, []byte(fmt.Sprintf("tx-%d", i)))
				}

				allMempoolTxs = append(allMempoolTxs, txs...)

				calls.Add(1)
				if calls.Load() == callsToCancel {
					cancel()
				}

				return &abci.ResponseReapTxs{Txs: txs}, nil
			})

		// Given mempool
		m := newTestAppMempool(t, config.DefaultMempoolConfig(), app)

		// ACT
		// stream txs from app
		sink := [][]byte{}
		ch := m.TxStream(ctx)

		for txs := range ch {
			sink = append(sink, txs.ToSliceOfBytes()...)
		}

		require.Subset(t, allMempoolTxs, sink)

		t.Run("logs failing reaps at Error once per interval and reports recovery", func(t *testing.T) {
			type reapFn = func(context.Context, *abci.RequestReapTxs) (*abci.ResponseReapTxs, error)
			appPanic := func(context.Context, *abci.RequestReapTxs) (*abci.ResponseReapTxs, error) {
				panic("app panic")
			}
			appErr := func(context.Context, *abci.RequestReapTxs) (*abci.ResponseReapTxs, error) {
				return nil, fmt.Errorf("connection error")
			}
			nilResp := func(context.Context, *abci.RequestReapTxs) (*abci.ResponseReapTxs, error) {
				return nil, nil
			}
			ctxErr := func(context.Context, *abci.RequestReapTxs) (*abci.ResponseReapTxs, error) {
				return nil, context.DeadlineExceeded
			}
			empty := func(context.Context, *abci.RequestReapTxs) (*abci.ResponseReapTxs, error) {
				return &abci.ResponseReapTxs{}, nil
			}
			recovery := slices.Repeat([]reapFn{empty}, reapRecoverySuccesses)

			for _, tt := range []struct {
				name            string
				reaps           []reapFn
				panicFirstError bool
				wantErrors      int
				wantStacks      int
				wantRecovered   []string
			}{
				{
					name: "healthy stream reports no recovery",
				},
				{
					name:          "successes must be in a row to end the streak",
					reaps:         slices.Concat([]reapFn{appErr}, recovery[:len(recovery)-1], []reapFn{appErr, nilResp, appErr}),
					wantErrors:    1,
					wantRecovered: []string{"failures=3"},
				},
				{
					name:          "panic after errors still logs its stack",
					reaps:         []reapFn{appErr, appErr, appPanic},
					wantErrors:    2,
					wantStacks:    1,
					wantRecovered: []string{"failures=3"},
				},
				{
					name:          "a panic streak after an error streak reports recovery",
					reaps:         slices.Concat([]reapFn{appErr}, recovery, []reapFn{appPanic}),
					wantErrors:    2,
					wantStacks:    1,
					wantRecovered: []string{"failures=1", "failures=1"},
				},
				{
					name:          "stacks stay throttled across streaks",
					reaps:         slices.Concat([]reapFn{appPanic}, recovery, []reapFn{appPanic}),
					wantErrors:    1,
					wantStacks:    1,
					wantRecovered: []string{"failures=1"},
				},
				{
					name:          "app context error does not stop the stream",
					reaps:         []reapFn{ctxErr},
					wantErrors:    1,
					wantRecovered: []string{"failures=1"},
				},
				{
					name:            "a logger that panics on a reap failure does not stop the stream",
					reaps:           []reapFn{appErr},
					panicFirstError: true,
					wantRecovered:   []string{"failures=1"},
				},
			} {
				t.Run(tt.name, func(t *testing.T) {
					cfg := config.TestMempoolConfig()
					cfg.ReapInterval = 5 * time.Millisecond

					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()

					batch := types.Txs{tx("after-failures")}

					var calls atomic.Int32
					app := abcimock.NewClient(t)
					app.On("ReapTxs", mock.Anything, mock.Anything).
						Return(func(ctx context.Context, req *abci.RequestReapTxs) (*abci.ResponseReapTxs, error) {
							switch i := int(calls.Add(1)) - 1; {
							case i < len(tt.reaps):
								return tt.reaps[i](ctx, req)
							case i < len(tt.reaps)+len(recovery):
								return empty(ctx, req)
							default:
								return &abci.ResponseReapTxs{Txs: batch.ToSliceOfBytes()}, nil
							}
						})

					var logs lockedBuffer
					logger := log.NewTMLogger(&logs)
					if tt.panicFirstError {
						logger = &firstErrorPanicsLogger{Logger: logger}
					}
					m := newTestAppMempool(t, cfg, app, WithAMLogger(logger))
					ch := m.TxStream(ctx)

					select {
					case txs, ok := <-ch:
						require.True(t, ok, "stream closed after a failed reap")
						require.Equal(t, batch, txs)
					case <-time.After(time.Second):
						t.Fatal("no txs streamed after the failing reaps")
					}

					cancel()
					for range ch {
					}

					out := logs.String()
					var errorLines int
					for _, line := range logLinesWith(out, "error reaping txs") {
						if strings.HasPrefix(line, "E[") {
							errorLines++
						}
					}
					var recovered []string
					for _, line := range logLinesWith(out, "reaping recovered") {
						if !strings.HasPrefix(line, "I[") {
							continue
						}
						for _, field := range strings.Fields(line) {
							if strings.HasPrefix(field, "failures=") {
								recovered = append(recovered, field)
							}
						}
					}

					require.Equal(t, tt.wantErrors, errorLines, out)
					require.Equal(t, tt.wantStacks, strings.Count(out, "stack="), out)
					require.Equal(t, tt.wantRecovered, recovered, out)
					if len(tt.reaps) == 0 {
						require.NotContains(t, out, "reaping recovered", out)
					}
					require.True(t, m.panicStacks.claim(), "reap failures must not use the one-off stack budget")
					require.True(t, m.txPanicStacks.claim(), "reap failures must not use the per-tx panic stack budget")
					require.True(t, m.checkTxErrorLogs.claim(), "reap failures must not use the CheckTx error log budget")
				})
			}
		})

		t.Run("each stream has its own reap log budgets", func(t *testing.T) {
			type streamKey struct{}
			cfg := config.TestMempoolConfig()
			cfg.ReapInterval = 5 * time.Millisecond

			var calls sync.Map
			app := abcimock.NewClient(t)
			app.On("ReapTxs", mock.Anything, mock.Anything).
				Return(func(ctx context.Context, _ *abci.RequestReapTxs) (*abci.ResponseReapTxs, error) {
					n, _ := calls.LoadOrStore(ctx.Value(streamKey{}), new(atomic.Int32))
					switch n.(*atomic.Int32).Add(1) {
					case 1:
						return nil, fmt.Errorf("connection error")
					case 2:
						panic("app panic")
					}
					return &abci.ResponseReapTxs{Txs: [][]byte{[]byte("reaped")}}, nil
				})

			var logs lockedBuffer
			m := newTestAppMempool(t, cfg, app, WithAMLogger(log.NewTMLogger(&logs)))

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			streams := []<-chan types.Txs{
				m.TxStream(context.WithValue(ctx, streamKey{}, 1)),
				m.TxStream(context.WithValue(ctx, streamKey{}, 2)),
			}
			for _, ch := range streams {
				select {
				case _, ok := <-ch:
					require.True(t, ok, "stream closed after a failed reap")
				case <-time.After(time.Second):
					t.Fatal("no txs streamed after the failed reaps")
				}
			}
			cancel()
			for _, ch := range streams {
				for range ch {
				}
			}

			out := logs.String()
			require.Equal(t, 2, strings.Count(out, "stack="), out)
			require.Len(t, logLinesWith(out, "E["), 4, out)
		})

		t.Run("a panic outside the app call closes the stream and logs at Error", func(t *testing.T) {
			cfg := config.TestMempoolConfig()
			cfg.ReapInterval = 5 * time.Millisecond

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			app := abcimock.NewClient(t)
			app.On("ReapTxs", mock.Anything, mock.Anything).Return(&abci.ResponseReapTxs{Txs: [][]byte{[]byte("reaped")}}, nil)

			metrics := NopMetrics()
			metrics.ReapedTxs = &panickingCounter{}
			var logs lockedBuffer
			m := newTestAppMempool(t, cfg, app, WithAMMetrics(metrics), WithAMLogger(log.NewTMLogger(&logs)))
			require.True(t, m.panicStacks.claim())

			select {
			case _, ok := <-m.TxStream(ctx):
				require.False(t, ok, "stream should close")
			case <-time.After(time.Second):
				t.Fatal("stream not closed after a panic in reapTxs")
			}

			requireOneErrorWithoutStack(t, logs.String(), "panic in AppMempool.reapTxs")
		})

		t.Run("app error after the stream context is canceled ends the stream quietly", func(t *testing.T) {
			cfg := config.TestMempoolConfig()
			cfg.ReapInterval = 5 * time.Millisecond

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			app := abcimock.NewClient(t)
			app.On("ReapTxs", mock.Anything, mock.Anything).
				Return(func(context.Context, *abci.RequestReapTxs) (*abci.ResponseReapTxs, error) {
					cancel()
					return nil, fmt.Errorf("connection closed")
				}).Once()

			var logs lockedBuffer
			ch := newTestAppMempool(t, cfg, app, WithAMLogger(log.NewTMLogger(&logs))).TxStream(ctx)

			select {
			case _, ok := <-ch:
				require.False(t, ok, "stream should close")
			case <-time.After(time.Second):
				t.Fatal("stream not closed after its context was canceled")
			}
			require.NotContains(t, logs.String(), "error reaping txs")
		})
	})
}

func TestAppMempool_UsesConfigValues(t *testing.T) {
	t.Run("ReapTxs receives MaxBytes and MaxGas from config", func(t *testing.T) {
		cfg := config.TestMempoolConfig()
		cfg.Type = config.MempoolTypeApp
		cfg.ReapMaxBytes = 12345
		cfg.ReapMaxGas = 67890
		cfg.ReapInterval = 10 * time.Millisecond // short for fast test

		var receivedReq *abci.RequestReapTxs
		app := abcimock.NewClient(t)
		app.On("ReapTxs", mock.Anything, mock.MatchedBy(func(req *abci.RequestReapTxs) bool {
			receivedReq = req
			return true
		})).Return(&abci.ResponseReapTxs{Txs: [][]byte{[]byte("tx1")}}, nil).Once()

		m := newTestAppMempool(t, cfg, app)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ch := m.TxStream(ctx)
		// Consume one batch to trigger ReapTxs call
		batch := <-ch
		require.Len(t, batch, 1)
		cancel()

		require.NotNil(t, receivedReq)
		require.Equal(t, uint64(12345), receivedReq.MaxBytes)
		require.Equal(t, uint64(67890), receivedReq.MaxGas)
	})

	t.Run("SeenCacheSize limits LRU cache", func(t *testing.T) {
		cfg := config.TestMempoolConfig()
		cfg.Type = config.MempoolTypeApp
		cfg.SeenCacheSize = 3 // small cache to test eviction

		app := abcimock.NewClient(t)
		app.On("InsertTx", mock.Anything, mock.Anything).
			Return(&abci.ResponseInsertTx{Code: abci.CodeTypeOK}, nil)
		app.On("CheckTx", mock.Anything, mock.Anything).
			Return(&abci.ResponseCheckTx{Code: abci.CodeTypeOK}, nil).Maybe() // not called by InsertTx

		m := newTestAppMempool(t, cfg, app)

		// Fill cache with 3 txs
		require.NoError(t, m.InsertTx(types.Tx("tx1")))
		require.NoError(t, m.InsertTx(types.Tx("tx2")))
		require.NoError(t, m.InsertTx(types.Tx("tx3")))

		// tx1 is seen - reject
		require.ErrorIs(t, m.InsertTx(types.Tx("tx1")), ErrSeenTx)

		// Insert 3 more to evict tx1, tx2, tx3 from LRU
		require.NoError(t, m.InsertTx(types.Tx("tx4")))
		require.NoError(t, m.InsertTx(types.Tx("tx5")))
		require.NoError(t, m.InsertTx(types.Tx("tx6")))

		// tx1 was evicted - should succeed now
		require.NoError(t, m.InsertTx(types.Tx("tx1")))
	})
}

// spyCounter implements go-kit metrics.Counter and records the Add() delta sum.
type spyCounter struct{ n atomic.Int64 }

func (c *spyCounter) With(_ ...string) kitmetrics.Counter { return c }
func (c *spyCounter) Add(delta float64)                   { c.n.Add(int64(delta)) }
func (c *spyCounter) value() int64                        { return c.n.Load() }

type panickingHistogram struct{}

func (h panickingHistogram) With(...string) kitmetrics.Histogram { return h }
func (panickingHistogram) Observe(float64)                       { panic("metrics panic") }

type panickingCounter struct{ spyCounter }

func (*panickingCounter) Add(float64) { panic("metrics panic") }

func TestAppMempool_FailureMetrics(t *testing.T) {
	type seenCheck int
	const (
		unchecked seenCheck = iota
		staysSeen
		forgottenAfterDelay
	)

	for _, tt := range []struct {
		name         string
		method       string
		ret          []any
		wantPanic    bool
		wantCode     uint32
		wantLog      string
		wantRejected int64
		wantFailed   int64
		seen         seenCheck
	}{
		{
			name:     "InsertTx retryable code",
			method:   "InsertTx",
			ret:      []any{&abci.ResponseInsertTx{Code: abci.CodeTypeRetry}, nil},
			wantCode: abci.CodeTypeRetry,
		},
		{
			name:         "InsertTx non-retryable code",
			method:       "InsertTx",
			ret:          []any{&abci.ResponseInsertTx{Code: 123}, nil},
			wantCode:     123,
			wantRejected: 1,
			seen:         staysSeen,
		},
		{
			name:       "InsertTx transport error",
			method:     "InsertTx",
			ret:        []any{nil, fmt.Errorf("connection error")},
			wantFailed: 1,
			seen:       forgottenAfterDelay,
		},
		{
			name:       "InsertTx nil response",
			method:     "InsertTx",
			ret:        []any{nil, nil},
			wantPanic:  true,
			wantFailed: 1,
			seen:       staysSeen,
		},
		{
			name:       "InsertTx app panic",
			method:     "InsertTx",
			ret:        []any{insertPanics},
			wantPanic:  true,
			wantFailed: 1,
			seen:       staysSeen,
		},
		{
			name:     "CheckTx non-retryable code",
			method:   "CheckTx",
			ret:      []any{&abci.ResponseCheckTx{Code: 123}, nil},
			wantCode: 123,
			seen:     forgottenAfterDelay,
		},
		{
			name:       "CheckTx transport error",
			method:     "CheckTx",
			ret:        []any{nil, fmt.Errorf("connection error")},
			wantCode:   abci.CodeTypeInternal,
			wantLog:    "CheckTx app client error: connection error",
			wantFailed: 1,
			seen:       forgottenAfterDelay,
		},
		{
			name:       "CheckTx nil response",
			method:     "CheckTx",
			ret:        []any{nil, nil},
			wantCode:   abci.CodeTypeInternal,
			wantLog:    "CheckTx app client error: nil CheckTx response",
			wantFailed: 1,
			seen:       forgottenAfterDelay,
		},
		{
			name:       "CheckTx app panic hides the panic value",
			method:     "CheckTx",
			ret:        []any{checkTxPanics},
			wantCode:   abci.CodeTypeInternal,
			wantLog:    "CheckTx app panic",
			wantFailed: 1,
			seen:       forgottenAfterDelay,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.TestMempoolConfig()
			cfg.CheckTxRetryDelay = 600 * time.Millisecond
			delay := cfg.CheckTxRetryDelay

			rejected, failed := &spyCounter{}, &spyCounter{}
			metrics := NopMetrics()
			metrics.RejectedTxs = rejected
			metrics.FailedTxs = failed

			app := abcimock.NewClient(t)
			app.On(tt.method, mock.Anything, mock.Anything).Return(tt.ret...)
			m := newTestAppMempool(t, cfg, app,
				WithAMMetrics(metrics), WithAMLogger(&firstErrorPanicsLogger{Logger: log.NewNopLogger()}))
			tx := types.Tx("metric-tx")

			call := func() { require.ErrorContains(t, m.InsertTx(tx), fmt.Sprintf("(code=%d)", tt.wantCode)) }
			if tt.method == "CheckTx" {
				call = func() {
					res := checkTxResult(t, m, tx)
					require.Equal(t, tt.wantCode, res.Code)
					require.Equal(t, tt.wantLog, res.Log)
				}
			}

			failedAt := time.Now()
			if tt.wantPanic {
				require.Panics(t, call)
			} else {
				require.NotPanics(t, call)
			}

			require.Equal(t, tt.wantRejected, rejected.value())
			require.Equal(t, tt.wantFailed, failed.value())

			forgotten := func() bool { return !m.guard.Has(tx.Key()) }
			switch tt.seen {
			case staysSeen:
				require.Never(t, forgotten, delay+delay/4, delay/10)
				require.ErrorIs(t, m.InsertTx(tx), ErrSeenTx)
			case forgottenAfterDelay:
				require.Never(t, func() bool { return forgotten() && time.Since(failedAt) < delay/2 }, delay/4, delay/40)
				require.Eventually(t, forgotten, 4*delay, delay/40)
			}
		})
	}
}
