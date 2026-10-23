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
	// One byte more than the cap can show once hex-encoded, so the truncation marker still appears.
	maxEncodedBytes = maxLoggedValueLen/2 + 1
)

// boundedString caps the formatted value because an app may put tx bytes (up to max_tx_bytes) in a panic or error.
func boundedString(v any) string {
	// Raw bytes (hex, as the default logger prints them) and types.Tx encode to two characters per byte, so they are
	// cut to what the cap can show before encoding. Other types are formatted in full: their own String,
	// Error or Format may not keep the bytes' prefix.
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

	// Back up at most to the start of one split rune, so a run of invalid bytes can't erase the value.
	cut := maxLoggedValueLen
	for i := 0; i < utf8.UTFMax-1 && !utf8.RuneStart(s[cut]); i++ {
		cut--
	}

	return s[:cut] + "...(truncated)"
}

// boundedValue defers boundedString until the logger formats the line, so filtered levels skip formatting.
type boundedValue struct{ v any }

func (b boundedValue) String() string { return boundedString(b.v) }

// txHashValue defers hashing until the logger formats the line, so filtered levels skip it.
type txHashValue types.Tx

func (h txHashValue) String() string { return fmt.Sprintf("%X", types.Tx(h).Hash()) }

// logThrottle hands out at most one claim per interval, e.g. for a panic stack or an Error line.
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
	logPanic(logger, stacks, false, msg, p, keyvals...)
}

// logRecoveredTxPanic is for panics a peer or RPC client can repeat per tx: only the line carrying
// the throttled stack is logged at Error, so they can't flood Error logs.
func logRecoveredTxPanic(logger log.Logger, stacks *logThrottle, msg string, p any, keyvals ...any) {
	logPanic(logger, stacks, true, msg, p, keyvals...)
}

// logRepeatable is for failures a peer or RPC client can repeat per tx: it logs at level at most once
// per interval and at Debug otherwise.
func logRepeatable(logger log.Logger, throttle *logThrottle, level logLevel, msg string, keyvals ...any) {
	logOrDebug(logger, throttle.claim(), level, msg, keyvals...)
}

func logPanic(logger log.Logger, stacks *logThrottle, debugWithoutStack bool, msg string, p any, keyvals ...any) {
	stack := stacks.stack()
	logErrorWithStack(logger, stack != nil || !debugWithoutStack, stack, msg, append([]any{"panic", boundedValue{p}}, keyvals...)...)
}

// logErrorWithStack logs at Error, with stack when there is one, if promote is set, and at Debug otherwise.
func logErrorWithStack(logger log.Logger, promote bool, stack []byte, msg string, keyvals ...any) {
	if promote && stack != nil {
		keyvals = append(keyvals, "stack", string(stack))
	}
	logOrDebug(logger, promote, log.Logger.Error, msg, keyvals...)
}

// logLevel is a log.Logger method expression such as log.Logger.Error.
type logLevel func(log.Logger, string, ...any)

// logOrDebug logs at level when promote is set and at Debug otherwise. It never panics: a broken
// logger must not escape the recover handler or loop that calls it.
func logOrDebug(logger log.Logger, promote bool, level logLevel, msg string, keyvals ...any) {
	defer func() { _ = recover() }()

	if promote {
		level(logger, msg, keyvals...)
		return
	}
	logger.Debug(msg, keyvals...)
}
