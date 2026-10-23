package mempool

import (
	"context"
	"fmt"
	"time"

	client "github.com/cometbft/cometbft/abci/client"
	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/internal/guard"
	"github.com/cometbft/cometbft/libs/log"
	"github.com/cometbft/cometbft/types"
	"github.com/pkg/errors"
)

// AppMempool represents a mempool that's implemented completely on the app-side via ABCI methods in opposite to
// concurrent-list mempool that stores transactions on comet's side. AppMempool only proxies requests to the app
// and broadcasts transactions to peers. Expectations are:
// - The app is expected to handle PreCheck, PostCheck, and Recheck by itself;
// - The mempool always returns 0 txs for ReapMaxBytesMaxGas as the app is expected to build the block;
// - It doesn't block other reactors for ABCI methods --> the app is expected to handle the mempool concurrently;
type AppMempool struct {
	ctx     context.Context
	config  *config.MempoolConfig
	metrics *Metrics
	app     AppMempoolClient

	// cache to avoid receiving the same txs from other peers.
	// supports TTL eviction policy.
	guard             *guard.Guard[types.TxKey]
	checkTxRetryDelay time.Duration

	logger           log.Logger
	panicStacks      *logThrottle
	txPanicStacks    *logThrottle
	checkTxErrorLogs *logThrottle
}

// AppMempoolClient is the interface for the app-side mempool.
type AppMempoolClient interface {
	// InsertTx inserts a tx into app-side mempool
	InsertTx(ctx context.Context, req *abci.RequestInsertTx) (*abci.ResponseInsertTx, error)

	// CheckTx checks a tx against the application
	CheckTx(ctx context.Context, req *abci.RequestCheckTx) (*abci.ResponseCheckTx, error)

	// ReapTxs reaps txs from app-side mempool
	ReapTxs(ctx context.Context, req *abci.RequestReapTxs) (*abci.ResponseReapTxs, error)

	// Flush app's connection
	Flush(context.Context) error
}

// AppMempoolOpt is the option for AppMempool
type AppMempoolOpt func(*AppMempool)

var _ Mempool = &AppMempool{}

var (
	ErrNotImplemented = errors.New("not implemented")
	ErrEmptyTx        = errors.New("tx is empty")
	ErrSeenTx         = errors.New("tx already seen")
)

func WithAMMetrics(metrics *Metrics) AppMempoolOpt {
	return func(m *AppMempool) { m.metrics = metrics }
}

func WithAMLogger(logger log.Logger) AppMempoolOpt {
	return func(m *AppMempool) { m.logger = logger }
}

func NewAppMempool(config *config.MempoolConfig, app AppMempoolClient, opts ...AppMempoolOpt) *AppMempool {
	m := &AppMempool{
		ctx:               context.Background(),
		config:            config,
		app:               app,
		guard:             guard.New[types.TxKey](config.SeenCacheSize),
		checkTxRetryDelay: config.CheckTxRetryDelay,
		metrics:           NopMetrics(),
		logger:            log.NewNopLogger(),
		panicStacks:       newLogThrottle(),
		txPanicStacks:     newLogThrottle(),
		checkTxErrorLogs:  newLogThrottle(),
	}

	for _, opt := range opts {
		opt(m)
	}

	if m.checkTxRetryDelay <= 0 {
		panic("mempool.CheckTxRetryDelay must be positive")
	}

	return m
}

// InsertTx inserts a tx into app-side mempool. The call is blocking, but thread-safe.
// Concurrent calls are expected and are caller's responsibility to handle.
// It panics on a nil response or an app panic and keeps the tx seen, so peers can't make the app re-process it.
func (m *AppMempool) InsertTx(tx types.Tx) error {
	if err := m.guardTx(tx); err != nil {
		return err
	}

	code, err := m.insertTx(tx)

	// todo (@swift1337): should we define more codes and handle them respectively?
	switch {
	case err != nil:
		return wrapErrCode("unable to insert tx", code, err)
	case codeRetry(code):
		// drop tx from seen cache (to retry later), but still return the error
		m.forgetTx(tx, true)
		return wrapErrCode("invalid code", code, err)
	case code != abci.CodeTypeOK:
		m.metrics.RejectedTxs.Add(1)
		return wrapErrCode("invalid code", code, err)
	default:
		m.metrics.TxSizeBytes.Observe(float64(len(tx)))
		return nil
	}
}

// guardTx guards the tx against empty and too large errors, and adds it to the seen cache.
func (m *AppMempool) guardTx(tx types.Tx) error {
	txSize := len(tx)

	if txSize == 0 {
		return ErrEmptyTx
	}

	if m.config.MaxTxBytes > 0 && txSize > m.config.MaxTxBytes {
		return &ErrTxTooLarge{
			Max:    m.config.MaxTxBytes,
			Actual: txSize,
		}
	}

	if !m.guard.Guard(tx.Key()) {
		m.metrics.AlreadyReceivedTxs.Add(1)
		return ErrSeenTx
	}

	return nil
}

// forgetTx forgets a tx after a delay
func (m *AppMempool) forgetTx(tx types.Tx, retryable bool) {
	delay := m.checkTxRetryDelay
	if retryable {
		delay /= 10
	}

	m.guard.ForgetAfter(tx.Key(), delay)
}

func (m *AppMempool) recordAppFailure(tx types.Tx) {
	m.metrics.FailedTxs.Add(1)
	m.forgetTx(tx, false)
}

func (m *AppMempool) insertTx(tx types.Tx) (uint32, error) {
	answered := false
	defer func() {
		if !answered {
			m.metrics.FailedTxs.Add(1)
		}
	}()

	// note: other ABCI methods panic if err is not nil
	resp, err := m.app.InsertTx(m.ctx, &abci.RequestInsertTx{Tx: tx})
	if err == nil && resp == nil {
		panic("nil InsertTx response")
	}
	answered = true

	if err != nil {
		m.recordAppFailure(tx)
	}

	return resp.GetCode(), err
}

// TxStream spins up a channel that streams valid transactions from app-side mempool.
// The expectation is that the caller would share it with other peers to gossip transactions.
// chan type is a list of txs, it is guaranteed to be non-empty.
func (m *AppMempool) TxStream(ctx context.Context) <-chan types.Txs {
	ch := make(chan types.Txs, 1)

	go func() {
		defer func() {
			if p := recover(); p != nil {
				logRecoveredPanic(m.logger, m.panicStacks, "panic in AppMempool.reapTxs", p)
			}
			close(ch)
		}()

		m.reapTxs(ctx, ch)
	}()

	return ch
}

type reapPanicError struct {
	value any
	stack []byte
}

func (e reapPanicError) Error() string {
	return "ReapTxs app panic: " + boundedString(e.value)
}

func (m *AppMempool) reapTxsOnce(ctx context.Context, req *abci.RequestReapTxs, stacks *logThrottle) (res *abci.ResponseReapTxs, err error) {
	defer func() {
		if p := recover(); p != nil {
			res, err = nil, reapPanicError{value: p, stack: stacks.stack()}
		}
	}()

	return m.app.ReapTxs(ctx, req)
}

const reapRecoverySuccesses = 10

type reapStreak struct {
	failures  int
	successes int
	reported  bool
}

func (m *AppMempool) logReapFailure(err error, streak *reapStreak, errorLogs *logThrottle) {
	var stack []byte
	var p reapPanicError
	if errors.As(err, &p) {
		stack = p.stack
	}

	promote := errorLogs.claim() || stack != nil
	streak.reported = streak.reported || promote
	logErrorWithStack(m.logger, promote, stack, "AppMempool.reapTxs: error reaping txs",
		"error", boundedValue{err}, "failures", streak.failures)
}

func (m *AppMempool) reapTxs(ctx context.Context, channel chan<- types.Txs) {
	req := &abci.RequestReapTxs{
		MaxBytes: m.config.ReapMaxBytes,
		MaxGas:   m.config.ReapMaxGas,
	}

	ticker := time.NewTicker(m.config.ReapInterval)
	defer ticker.Stop()

	var streak reapStreak
	reapPanicStacks, reapErrorLogs := newLogThrottle(), newLogThrottle()

	for {
		select {
		case <-ctx.Done():
			m.logger.Debug("AppMempool.reapTxs: context is done")
			return
		case <-ticker.C:
			res, err := m.reapTxsOnce(ctx, req, reapPanicStacks)
			if err != nil {
				if ctx.Err() != nil {
					m.logger.Debug("AppMempool.reapTxs: context done while reaping txs")
					return
				}

				streak.failures++
				streak.successes = 0
				m.logReapFailure(err, &streak, reapErrorLogs)
				continue
			}

			if streak.failures > 0 {
				streak.successes++
				if streak.successes >= reapRecoverySuccesses {
					logOrDebug(m.logger, streak.reported, log.Logger.Info, "AppMempool.reapTxs: reaping recovered", "failures", streak.failures)
					streak = reapStreak{}
				}
			}

			if res == nil || len(res.Txs) == 0 {
				// no txs to send
				continue
			}

			txs := types.ToTxs(res.Txs)
			m.metrics.ReapedTxs.Add(float64(len(txs)))

			select {
			case <-ctx.Done():
				m.logger.Debug("AppMempool.reapTxs: context done while streaming txs")
				return
			case channel <- txs:
				// all good
			}

			// avoid receiving these txs again from other peers.
			for _, tx := range txs {
				m.guard.Guard(tx.Key())
			}
		}
	}
}

// Close stops the background goroutine started by the seen-cache guard.
// Must be called when the mempool is no longer needed.
func (m *AppMempool) Close() {
	m.guard.Close()
}

// FlushAppConn flushes app client (copied from CListMempool)
func (m *AppMempool) FlushAppConn() error {
	err := m.app.Flush(m.ctx)
	if err != nil {
		return ErrFlushAppConn{Err: err}
	}

	return nil
}

// CheckTx calls ABCI.CheckTx
// This type of mempool assumes ABCI.CheckTx is safe to call LOCK-FREE.
// It's a responsibility of application to handle concurrency safely.
func (m *AppMempool) CheckTx(tx types.Tx, callback func(res *abci.ResponseCheckTx), _ TxInfo) error {
	if err := m.guardTx(tx); err != nil {
		return err
	}

	ctx := client.LockFreeContext(m.ctx)

	go func() {
		defer func() {
			if p := recover(); p != nil {
				logRecoveredPanic(m.logger, m.panicStacks, "panic in the AppMempool CheckTx goroutine", p, "tx", txHashValue(tx))
			}
		}()

		res := m.checkTx(ctx, tx)
		if callback != nil {
			callback(res)
		}
	}()

	return nil
}

func (m *AppMempool) checkTx(ctx context.Context, tx types.Tx) (res *abci.ResponseCheckTx) {
	defer func() {
		if p := recover(); p != nil {
			m.recordAppFailure(tx)
			res = &abci.ResponseCheckTx{Code: abci.CodeTypeInternal, Log: "CheckTx app panic"}
			logRecoveredTxPanic(m.logger, m.txPanicStacks, "panic in AppMempool.CheckTx", p, "tx", txHashValue(tx))
		}
	}()

	res, err := m.app.CheckTx(ctx, &abci.RequestCheckTx{Tx: tx})
	if err == nil && res == nil {
		err = errors.New("nil CheckTx response")
	}
	if err != nil {
		m.recordAppFailure(tx)
		errMsg := boundedString(err)
		logOrDebug(m.logger, m.checkTxErrorLogs.claim(), log.Logger.Error, "AppMempool.CheckTx: error checking tx", "error", errMsg, "tx", txHashValue(tx))
		return &abci.ResponseCheckTx{Code: abci.CodeTypeInternal, Log: "CheckTx app client error: " + errMsg}
	}

	// handle (non)retryable errors:
	// allow RPC requests to be retryable while keeping DDoS vector small.
	//
	// - if app returns a retryable error code -> forget after a small delay
	// - if app returns a non-retryable error code -> forget after a bigger delay
	if res.Code != abci.CodeTypeOK {
		m.forgetTx(tx, codeRetry(res.Code))
	}

	return res
}

// Update does nothing for an app mempool
func (m *AppMempool) Update(_ int64, _ types.Txs, _ []*abci.ExecTxResult, _ PreCheckFunc, _ PostCheckFunc) error {
	return nil
}

// reading from this channel will block forever, which is fine for an app mempool
func (m *AppMempool) TxsAvailable() <-chan struct{} { return nil }
func (m *AppMempool) EnableTxsAvailable()           {}

func (m *AppMempool) Size() int        { return 0 }
func (m *AppMempool) SizeBytes() int64 { return 0 }

func (m *AppMempool) ReapMaxBytesMaxGas(_, _ int64) types.Txs { return nil }
func (m *AppMempool) ReapMaxTxs(_ int) types.Txs              { return nil }
func (m *AppMempool) RemoveTxByKey(_ types.TxKey) error       { return nil }
func (m *AppMempool) Flush()                                  {}

func (m *AppMempool) Lock()   {}
func (m *AppMempool) Unlock() {}

func codeRetry(code uint32) bool {
	return code >= abci.CodeTypeRetry
}

func wrapErrCode(msg string, code uint32, err error) error {
	if err == nil {
		return fmt.Errorf("%s: (code=%d)", msg, code)
	}

	return errors.Wrapf(err, "%s: (code=%d)", msg, code)
}
