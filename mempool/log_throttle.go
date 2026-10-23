package mempool

import (
	"fmt"
	"runtime/debug"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/cometbft/cometbft/libs/log"
	"github.com/cometbft/cometbft/types"
)

const (
	logThrottleInterval = time.Minute
	maxLoggedValueLen   = 1024
	maxEncodedBytes     = maxLoggedValueLen/2 + 1
)

func boundedString(v any) string {
	var s string
	switch v := v.(type) {
	case string:
		s = v
	case []byte:
		s = fmt.Sprintf("%X", v[:min(len(v), maxEncodedBytes)])
	case types.Tx:
		s = v[:min(len(v), maxEncodedBytes)].String()
	default:
		s = fmt.Sprint(v)
	}
	if len(s) <= maxLoggedValueLen {
		return s
	}

	cut := maxLoggedValueLen
	for i := 0; i < utf8.UTFMax-1 && !utf8.RuneStart(s[cut]); i++ {
		cut--
	}

	return s[:cut] + "...(truncated)"
}

type boundedValue struct{ v any }

func (b boundedValue) String() string { return boundedString(b.v) }

type txHashValue types.Tx

func (h txHashValue) String() string { return fmt.Sprintf("%X", types.Tx(h).Hash()) }

type logThrottle struct {
	elapsed func() time.Duration
	nextAt  atomic.Int64
}

func newLogThrottle() *logThrottle {
	start := time.Now()
	return &logThrottle{elapsed: func() time.Duration { return time.Since(start) }}
}

func (t *logThrottle) claim() bool {
	now := int64(t.elapsed())
	next := t.nextAt.Load()

	return now >= next && t.nextAt.CompareAndSwap(next, now+int64(logThrottleInterval))
}

func (t *logThrottle) stack() []byte {
	if !t.claim() {
		return nil
	}

	return debug.Stack()
}

func logRecoveredPanic(logger log.Logger, stacks *logThrottle, msg string, p any, keyvals ...any) {
	logErrorWithStack(logger, true, stacks.stack(), msg, append([]any{"panic", boundedValue{p}}, keyvals...)...)
}

func logRecoveredTxPanic(logger log.Logger, stacks *logThrottle, msg string, p any, keyvals ...any) {
	logErrorWithStack(logger, false, stacks.stack(), msg, append([]any{"panic", boundedValue{p}}, keyvals...)...)
}

func logErrorWithStack(logger log.Logger, promote bool, stack []byte, msg string, keyvals ...any) {
	if stack != nil {
		promote = true
		keyvals = append(keyvals, "stack", string(stack))
	}
	logOrDebug(logger, promote, log.Logger.Error, msg, keyvals...)
}

func logOrDebug(logger log.Logger, promote bool, level func(log.Logger, string, ...any), msg string, keyvals ...any) {
	defer func() { _ = recover() }()

	if promote {
		level(logger, msg, keyvals...)
		return
	}
	logger.Debug(msg, keyvals...)
}
