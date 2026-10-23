package mempool

import (
	"context"
	"fmt"
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

	const (
		codeFail       = uint32(123)
		testRetryDelay = 600 * time.Millisecond
	)

	requireSeenPastRetryDelay := func(t *testing.T, m *AppMempool, tx types.Tx) {
		delay := m.checkTxRetryDelay
		require.Never(t, func() bool { return !m.guard.Has(tx.Key()) }, delay+delay/4, delay/10)
		require.ErrorIs(t, m.InsertTx(tx), ErrSeenTx)
	}

	requireForgottenAfterRetryDelay := func(t *testing.T, m *AppMempool, tx types.Tx, failedAt time.Time) {
		delay := m.checkTxRetryDelay
		forgotten := func() bool { return !m.guard.Has(tx.Key()) }
		// Only a forget before half the delay fails, so a stalled test can't mistake an on-time forget for an early one.
		require.Never(t, func() bool { return forgotten() && time.Since(failedAt) < delay/2 }, delay/4, delay/40)
		require.Eventually(t, forgotten, 4*delay, delay/40)
	}

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

		t.Run("keeps inserted tx in seen cache past CheckTxRetryDelay", func(t *testing.T) {
			cfg := config.TestMempoolConfig()
			cfg.CheckTxRetryDelay = testRetryDelay

			app := abcimock.NewClient(t)
			app.On("InsertTx", mock.Anything, mock.Anything).
				Return(&abci.ResponseInsertTx{Code: abci.CodeTypeOK}, nil).Once()

			m := newTestAppMempool(t, cfg, app)
			insertedTx := tx("inserted-tx")

			require.NoError(t, m.InsertTx(insertedTx))
			requireSeenPastRetryDelay(t, m, insertedTx)
		})

		t.Run("keeps a tx rejected with a non-retryable code in seen cache", func(t *testing.T) {
			cfg := config.TestMempoolConfig()
			cfg.CheckTxRetryDelay = testRetryDelay

			app := abcimock.NewClient(t)
			app.On("InsertTx", mock.Anything, mock.Anything).
				Return(&abci.ResponseInsertTx{Code: codeFail}, nil).Once()

			m := newTestAppMempool(t, cfg, app)
			rejectedTx := tx("rejected-tx")

			require.Error(t, m.InsertTx(rejectedTx))
			requireSeenPastRetryDelay(t, m, rejectedTx)
		})

		t.Run("forgets tx after CheckTxRetryDelay on transport error", func(t *testing.T) {
			cfg := config.TestMempoolConfig()
			cfg.CheckTxRetryDelay = testRetryDelay

			app := abcimock.NewClient(t)
			app.On("InsertTx", mock.Anything, mock.Anything).
				Return((*abci.ResponseInsertTx)(nil), fmt.Errorf("connection error"))

			m := newTestAppMempool(t, cfg, app)
			tx := types.Tx("retry-tx")

			failedAt := time.Now()
			require.Error(t, m.InsertTx(tx))
			require.ErrorIs(t, m.InsertTx(tx), ErrSeenTx)
			requireForgottenAfterRetryDelay(t, m, tx, failedAt)
		})

		t.Run("keeps a tx that panics the app in seen cache", func(t *testing.T) {
			for _, tt := range []struct {
				name     string
				insertTx func(context.Context, *abci.RequestInsertTx) (*abci.ResponseInsertTx, error)
			}{
				{
					name: "nil response",
					insertTx: func(context.Context, *abci.RequestInsertTx) (*abci.ResponseInsertTx, error) {
						return nil, nil
					},
				},
				{
					name:     "app panic",
					insertTx: insertPanics,
				},
			} {
				t.Run(tt.name, func(t *testing.T) {
					cfg := config.TestMempoolConfig()
					cfg.CheckTxRetryDelay = testRetryDelay

					app := abcimock.NewClient(t)
					app.On("InsertTx", mock.Anything, mock.Anything).Return(tt.insertTx)

					m := newTestAppMempool(t, cfg, app)
					panicTx := tx("panic-tx")

					require.Panics(t, func() { _ = m.InsertTx(panicTx) })
					requireSeenPastRetryDelay(t, m, panicTx)
				})
			}
		})

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
			tx := types.Tx("panic-tx")

			require.NoError(t, m.CheckTx(tx, func(_ *abci.ResponseCheckTx) { panic("callback panic") }, TxInfo{}))
			require.Eventually(t, func() bool {
				return !m.guard.Has(tx.Key())
			}, cfg.CheckTxRetryDelay, 5*time.Millisecond)

			require.Eventually(t, func() bool {
				return strings.Contains(logs.String(), "panic in the AppMempool CheckTx goroutine")
			}, time.Second, 5*time.Millisecond)
			requireOneErrorWithStack(t, logs.String(), "panic in the AppMempool CheckTx goroutine")
			require.False(t, m.panicStacks.claim(), "a CheckTx goroutine panic uses the one-off stack budget")
			require.True(t, m.txPanicStacks.claim(), "a CheckTx goroutine panic must not use the per-tx panic stack budget")
		})

		t.Run("app panic answers and forgets tx even if logging it panics", func(t *testing.T) {
			cfg := config.TestMempoolConfig()
			cfg.CheckTxRetryDelay = testRetryDelay

			app := abcimock.NewClient(t)
			app.On("CheckTx", mock.Anything, mock.Anything).
				Return(checkTxPanics)

			m := newTestAppMempool(t, cfg, app, WithAMLogger(&firstErrorPanicsLogger{Logger: log.NewNopLogger()}))
			panicTx := tx("panic-log-tx")

			failedAt := time.Now()
			res := checkTxResult(t, m, panicTx)
			require.Equal(t, abci.CodeTypeInternal, res.Code)
			requireForgottenAfterRetryDelay(t, m, panicTx, failedAt)
		})

		t.Run("app panic with a panicking String still answers the callback", func(t *testing.T) {
			app := abcimock.NewClient(t)
			app.On("CheckTx", mock.Anything, mock.Anything).
				Return(func(context.Context, *abci.RequestCheckTx) (*abci.ResponseCheckTx, error) {
					panic(panickingStringer{})
				})

			var logs lockedBuffer
			m := newTestAppMempool(t, config.TestMempoolConfig(), app, WithAMLogger(log.NewTMLogger(&logs)))

			res := checkTxResult(t, m, tx("bad-stringer-tx"))
			require.Equal(t, abci.CodeTypeInternal, res.Code)

			out := logs.String()
			require.Contains(t, out, "panic in AppMempool.CheckTx", out)
			require.Contains(t, out, "String panic", out)
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
			require.True(t, m.panicStacks.claim(), "CheckTx panics must not use the one-off stack budget")
			require.True(t, m.checkTxErrorLogs.claim(), "CheckTx panics must not use the app error log budget")
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

		t.Run("app error is answered and counted once even if logging it panics", func(t *testing.T) {
			app := abcimock.NewClient(t)
			app.On("CheckTx", mock.Anything, mock.Anything).Return((*abci.ResponseCheckTx)(nil), fmt.Errorf("connection error"))

			failed := &spyCounter{}
			metrics := NopMetrics()
			metrics.FailedTxs = failed
			m := newTestAppMempool(t, config.TestMempoolConfig(), app,
				WithAMMetrics(metrics), WithAMLogger(&firstErrorPanicsLogger{Logger: log.NewNopLogger()}))

			res := checkTxResult(t, m, tx("error-log-panics"))
			require.Equal(t, abci.CodeTypeInternal, res.Code)
			require.Contains(t, res.Log, "CheckTx app client error")
			require.Equal(t, int64(1), failed.value())
		})

		t.Run("app failure calls callback with internal code and forgets tx after CheckTxRetryDelay", func(t *testing.T) {
			for _, tt := range []struct {
				name    string
				checkTx func(context.Context, *abci.RequestCheckTx) (*abci.ResponseCheckTx, error)
				wantLog string
			}{
				{
					name: "client error",
					checkTx: func(context.Context, *abci.RequestCheckTx) (*abci.ResponseCheckTx, error) {
						return nil, fmt.Errorf("connection error")
					},
					wantLog: "CheckTx app client error: connection error",
				},
				{
					name: "nil response",
					checkTx: func(context.Context, *abci.RequestCheckTx) (*abci.ResponseCheckTx, error) {
						return nil, nil
					},
					wantLog: "CheckTx app client error: nil CheckTx response",
				},
				{
					name: "app panic hides the panic value",
					checkTx: func(context.Context, *abci.RequestCheckTx) (*abci.ResponseCheckTx, error) {
						panic("app internals")
					},
					wantLog: "CheckTx app panic",
				},
			} {
				t.Run(tt.name, func(t *testing.T) {
					cfg := config.TestMempoolConfig()
					cfg.CheckTxRetryDelay = testRetryDelay

					app := abcimock.NewClient(t)
					app.On("CheckTx", mock.Anything, mock.Anything).Return(tt.checkTx)

					m := newTestAppMempool(t, cfg, app)
					failedTx := tx("app-failure-tx")

					failedAt := time.Now()
					res := checkTxResult(t, m, failedTx)
					require.Equal(t, abci.CodeTypeInternal, res.Code)
					require.Equal(t, tt.wantLog, res.Log)
					requireForgottenAfterRetryDelay(t, m, failedTx, failedAt)
				})
			}
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
			recovery := make([]reapFn, reapRecoverySuccesses)
			for i := range recovery {
				recovery[i] = empty
			}

			for _, tt := range []struct {
				name          string
				reaps         []reapFn
				wantErrors    int
				wantStacks    int
				wantRecovered []string
			}{
				{
					name:          "panic streak logs one Error with its stack",
					reaps:         []reapFn{appPanic, appPanic, appPanic, appPanic, appPanic},
					wantErrors:    1,
					wantStacks:    1,
					wantRecovered: []string{"failures=5"},
				},
				{
					name:          "one success between failures keeps the streak",
					reaps:         []reapFn{appErr, appErr, appErr, empty, appErr},
					wantErrors:    1,
					wantRecovered: []string{"failures=4"},
				},
				{
					name:          "a later streak within the interval logs at Debug and reports no recovery",
					reaps:         append(append([]reapFn{appErr}, recovery...), appErr),
					wantErrors:    1,
					wantRecovered: []string{"failures=1"},
				},
				{
					name:          "successes must be in a row to end the streak",
					reaps:         append(append(append([]reapFn{appErr}, recovery[:len(recovery)-1]...), appErr, empty), appErr),
					wantErrors:    1,
					wantRecovered: []string{"failures=3"},
				},
				{
					name: "healthy stream logs nothing",
				},
				{
					name:          "panic after errors still logs its stack",
					reaps:         []reapFn{appErr, appErr, appPanic},
					wantErrors:    2,
					wantStacks:    1,
					wantRecovered: []string{"failures=3"},
				},
				{
					name:          "stacks stay throttled across streaks",
					reaps:         append(append([]reapFn{appPanic}, recovery...), appPanic),
					wantErrors:    1,
					wantStacks:    1,
					wantRecovered: []string{"failures=1"},
				},
				{
					name:          "nil response is an empty reap",
					reaps:         []reapFn{appPanic, nilResp},
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
					require.True(t, m.panicStacks.claim(), "reap failures must not use the one-off stack budget")
					require.True(t, m.txPanicStacks.claim(), "reap failures must not use the per-tx panic stack budget")
					require.True(t, m.checkTxErrorLogs.claim(), "reap failures must not use the CheckTx error log budget")
				})
			}
		})

		t.Run("a logger that panics on a reap failure does not stop the stream", func(t *testing.T) {
			cfg := config.TestMempoolConfig()
			cfg.ReapInterval = 5 * time.Millisecond

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			batch := types.Txs{tx("after-logger-panic")}
			app := abcimock.NewClient(t)
			app.On("ReapTxs", mock.Anything, mock.Anything).Return((*abci.ResponseReapTxs)(nil), fmt.Errorf("connection error")).Once()
			app.On("ReapTxs", mock.Anything, mock.Anything).Return(&abci.ResponseReapTxs{Txs: batch.ToSliceOfBytes()}, nil)

			m := newTestAppMempool(t, cfg, app, WithAMLogger(&firstErrorPanicsLogger{Logger: log.NewNopLogger()}))
			ch := m.TxStream(ctx)

			select {
			case txs, ok := <-ch:
				require.True(t, ok, "stream closed after a logger panic")
				require.Equal(t, batch, txs)
			case <-time.After(time.Second):
				t.Fatal("no txs streamed after the logger panic")
			}
		})

		t.Run("each stream has its own reap log budgets", func(t *testing.T) {
			type streamKey struct{}
			cfg := config.TestMempoolConfig()
			cfg.ReapInterval = 5 * time.Millisecond

			var panicked sync.Map
			app := abcimock.NewClient(t)
			app.On("ReapTxs", mock.Anything, mock.Anything).
				Return(func(ctx context.Context, _ *abci.RequestReapTxs) (*abci.ResponseReapTxs, error) {
					if _, loaded := panicked.LoadOrStore(ctx.Value(streamKey{}), true); !loaded {
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
					require.True(t, ok, "stream closed after a reap panic")
				case <-time.After(time.Second):
					t.Fatal("no txs streamed after the reap panic")
				}
			}
			cancel()
			for _, ch := range streams {
				for range ch {
				}
			}

			out := logs.String()
			require.Equal(t, 2, strings.Count(out, "stack="), "each stream logs its first panic stack: %s", out)
			require.Len(t, logLinesWith(out, "E["), 2, out)
		})

		t.Run("a panic outside the app call closes the stream and logs its stack", func(t *testing.T) {
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

			select {
			case _, ok := <-m.TxStream(ctx):
				require.False(t, ok, "stream should close")
			case <-time.After(time.Second):
				t.Fatal("stream not closed after a panic in reapTxs")
			}

			requireOneErrorWithStack(t, logs.String(), "panic in AppMempool.reapTxs")
			require.False(t, m.panicStacks.claim(), "a stream panic uses the one-off stack budget")
			require.True(t, m.txPanicStacks.claim(), "a stream panic must not use the per-tx panic stack budget")
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

		t.Run("stream context done during a reap ends the stream quietly", func(t *testing.T) {
			cfg := config.TestMempoolConfig()
			cfg.ReapInterval = 5 * time.Millisecond

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			app := abcimock.NewClient(t)
			app.On("ReapTxs", mock.Anything, mock.Anything).
				Return(func(ctx context.Context, _ *abci.RequestReapTxs) (*abci.ResponseReapTxs, error) {
					cancel()
					return nil, ctx.Err()
				}).Once()

			var logs lockedBuffer
			logger := log.NewTMLogger(&logs)
			ch := newTestAppMempool(t, cfg, app, WithAMLogger(logger)).TxStream(ctx)

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
	type insertFn = func(context.Context, *abci.RequestInsertTx) (*abci.ResponseInsertTx, error)
	insertResponds := func(code uint32) insertFn {
		return func(context.Context, *abci.RequestInsertTx) (*abci.ResponseInsertTx, error) {
			return &abci.ResponseInsertTx{Code: code}, nil
		}
	}

	for _, tt := range []struct {
		name         string
		method       string
		ret          any
		wantPanic    bool
		wantRejected int64
		wantFailed   int64
	}{
		{name: "InsertTx ok", method: "InsertTx", ret: insertResponds(abci.CodeTypeOK)},
		{name: "InsertTx retryable code", method: "InsertTx", ret: insertResponds(abci.CodeTypeRetry)},
		{name: "InsertTx non-retryable code", method: "InsertTx", ret: insertResponds(123), wantRejected: 1},
		{
			name:   "InsertTx transport error",
			method: "InsertTx",
			ret: func(context.Context, *abci.RequestInsertTx) (*abci.ResponseInsertTx, error) {
				return nil, fmt.Errorf("connection error")
			},
			wantFailed: 1,
		},
		{
			name:   "InsertTx nil response",
			method: "InsertTx",
			ret: func(context.Context, *abci.RequestInsertTx) (*abci.ResponseInsertTx, error) {
				return nil, nil
			},
			wantPanic:  true,
			wantFailed: 1,
		},
		{
			name:       "InsertTx app panic",
			method:     "InsertTx",
			ret:        insertPanics,
			wantPanic:  true,
			wantFailed: 1,
		},
		{
			name:   "CheckTx non-retryable code",
			method: "CheckTx",
			ret: func(context.Context, *abci.RequestCheckTx) (*abci.ResponseCheckTx, error) {
				return &abci.ResponseCheckTx{Code: 123}, nil
			},
		},
		{
			name:   "CheckTx transport error",
			method: "CheckTx",
			ret: func(context.Context, *abci.RequestCheckTx) (*abci.ResponseCheckTx, error) {
				return nil, fmt.Errorf("connection error")
			},
			wantFailed: 1,
		},
		{
			name:   "CheckTx nil response",
			method: "CheckTx",
			ret: func(context.Context, *abci.RequestCheckTx) (*abci.ResponseCheckTx, error) {
				return nil, nil
			},
			wantFailed: 1,
		},
		{
			name:       "CheckTx app panic",
			method:     "CheckTx",
			ret:        checkTxPanics,
			wantFailed: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rejected, failed := &spyCounter{}, &spyCounter{}
			metrics := NopMetrics()
			metrics.RejectedTxs = rejected
			metrics.FailedTxs = failed

			app := abcimock.NewClient(t)
			app.On(tt.method, mock.Anything, mock.Anything).Return(tt.ret)
			m := newTestAppMempool(t, config.TestMempoolConfig(), app, WithAMMetrics(metrics))

			call := func() { _ = m.InsertTx(types.Tx("metric-tx")) }
			if tt.method == "CheckTx" {
				call = func() { checkTxResult(t, m, types.Tx("metric-tx")) }
			}

			if tt.wantPanic {
				require.Panics(t, call)
			} else {
				require.NotPanics(t, call)
			}

			require.Equal(t, tt.wantRejected, rejected.value())
			require.Equal(t, tt.wantFailed, failed.value())
		})
	}
}
