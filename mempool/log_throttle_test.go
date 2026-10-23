package mempool

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	abcimock "github.com/cometbft/cometbft/abci/client/mocks"
	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/libs/log"
	"github.com/cometbft/cometbft/types"
)

// requireFirstAtRestDebug asserts the first msg line starts with prefix and, when Debug logs are compiled
// in, that the only other one is at Debug.
func requireFirstAtRestDebug(t *testing.T, out, msg, prefix string) []string {
	t.Helper()
	lines := logLinesWith(out, msg)
	require.NotEmpty(t, lines, out)
	require.True(t, strings.HasPrefix(lines[0], prefix), out)
	if log.LogDebug {
		require.Len(t, lines, 2, out)
		require.True(t, strings.HasPrefix(lines[1], "D["), out)
	} else {
		require.Len(t, lines, 1, out)
	}
	return lines
}

func logLinesWith(out, msg string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, msg) {
			lines = append(lines, line)
		}
	}
	return lines
}

func insertPanics(context.Context, *abci.RequestInsertTx) (*abci.ResponseInsertTx, error) {
	panic("app panic")
}

func checkTxPanics(context.Context, *abci.RequestCheckTx) (*abci.ResponseCheckTx, error) {
	panic("app panic")
}

// requireOneErrorWithStack asserts msg was logged once, at Error and with a stack.
func requireOneErrorWithStack(t *testing.T, out, msg string) {
	t.Helper()
	lines := logLinesWith(out, msg)
	require.Len(t, lines, 1, out)
	require.True(t, strings.HasPrefix(lines[0], "E["), out)
	require.Contains(t, lines[0], "stack=", out)
}

// recordingLogger keeps the keyvals of the last Debug line per message, so tests can check value types.
type recordingLogger struct {
	log.Logger
	mu    sync.Mutex
	debug map[string][]any
}

func (l *recordingLogger) Debug(msg string, keyvals ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.debug == nil {
		l.debug = map[string][]any{}
	}
	l.debug[msg] = keyvals
}

func (l *recordingLogger) value(msg, key string) any {
	l.mu.Lock()
	defer l.mu.Unlock()
	keyvals := l.debug[msg]
	for i := 0; i+1 < len(keyvals); i += 2 {
		if keyvals[i] == key {
			return keyvals[i+1]
		}
	}
	return nil
}

type lengthFormatter []byte

func (b lengthFormatter) Format(f fmt.State, _ rune) { fmt.Fprint(f, len(b)) }

type lengthStringer []byte

func (b lengthStringer) String() string { return fmt.Sprint(len(b)) }

func TestLogThrottle(t *testing.T) {
	newThrottle := func(now *time.Duration) *logThrottle {
		return &logThrottle{elapsed: func() time.Duration { return *now }}
	}

	t.Run("re-arms after the interval", func(t *testing.T) {
		var now time.Duration
		th := newThrottle(&now)

		require.True(t, th.claim(), "first panic logs its stack")
		require.False(t, th.claim())

		now += logThrottleInterval - time.Second
		require.False(t, th.claim())

		now += time.Second
		require.True(t, th.claim(), "stack logs again once the interval has passed")
		require.False(t, th.claim())
	})

	t.Run("concurrent panics claim one stack per interval", func(t *testing.T) {
		var now time.Duration
		th := newThrottle(&now)

		for round := 0; round < 200; round++ {
			now += logThrottleInterval

			var (
				wins  atomic.Int32
				start = make(chan struct{})
				wg    sync.WaitGroup
			)
			for i := 0; i < 64; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					if th.claim() {
						wins.Add(1)
					}
				}()
			}
			close(start)
			wg.Wait()

			require.Equal(t, int32(1), wins.Load(), "round %d", round)
		}
	})
}

type firstErrorPanicsLogger struct {
	log.Logger
	panicked atomic.Bool
}

func (l *firstErrorPanicsLogger) Error(msg string, keyvals ...any) {
	if l.panicked.CompareAndSwap(false, true) {
		panic("logger panic")
	}
	l.Logger.Error(msg, keyvals...)
}

type countingStringer struct{ n *atomic.Int32 }

func (c countingStringer) String() string {
	c.n.Add(1)
	return "counted"
}

type panickingStringer struct{}

func (panickingStringer) String() string { panic("String panic") }

func TestBoundedString(t *testing.T) {
	long := strings.Repeat("x", 10*maxLoggedValueLen)

	for _, tt := range []struct {
		name string
		p    any
		want string
	}{
		{name: "short value is kept", p: "app panic", want: "app panic"},
		{name: "value at the cap is kept", p: long[:maxLoggedValueLen], want: long[:maxLoggedValueLen]},
		{name: "long value is truncated", p: long, want: long[:maxLoggedValueLen] + "...(truncated)"},
		{
			name: "truncation drops a split rune",
			p:    strings.Repeat("x", maxLoggedValueLen-1) + "é" + long,
			want: strings.Repeat("x", maxLoggedValueLen-1) + "...(truncated)",
		},
		{
			name: "truncation keeps invalid bytes before the cut",
			p:    "\xff" + long,
			want: ("\xff" + long)[:maxLoggedValueLen] + "...(truncated)",
		},
		{name: "short bytes are hex-encoded", p: []byte("ab"), want: "6162"},
		{
			name: "long bytes are cut before formatting",
			p:    []byte(long),
			want: fmt.Sprintf("%X", []byte(long))[:maxLoggedValueLen] + "...(truncated)",
		},
		{
			name: "a run of invalid bytes at the cut keeps the value",
			p:    "a" + strings.Repeat("\x80", 2*maxLoggedValueLen),
			want: ("a" + strings.Repeat("\x80", 2*maxLoggedValueLen))[:maxLoggedValueLen-utf8.UTFMax+1] + "...(truncated)",
		},
		{
			name: "named byte slices are cut before formatting",
			p:    types.Tx(long),
			want: fmt.Sprint(types.Tx(long))[:maxLoggedValueLen] + "...(truncated)",
		},
		{
			name: "byte slices with their own String are formatted in full first",
			p:    lengthStringer(long),
			want: fmt.Sprint(len(long)),
		},
		{
			name: "byte slices with their own Format are formatted in full first",
			p:    lengthFormatter(long),
			want: fmt.Sprint(len(long)),
		},
		{name: "panicking String is formatted", p: panickingStringer{}, want: "%!v(PANIC=String method: String panic)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, boundedString(tt.p))
		})
	}
}

func TestLazyLogValues(t *testing.T) {
	t.Run("tx hash values print the uppercase hex hash", func(t *testing.T) {
		tx := types.Tx("hashed-tx")
		require.Equal(t, fmt.Sprintf("%X", tx.Hash()), fmt.Sprint(txHashValue(tx)))
	})

	t.Run("filtered log lines skip formatting", func(t *testing.T) {
		var formatted atomic.Int32
		v := countingStringer{n: &formatted}

		var logs lockedBuffer
		logger := log.NewFilter(log.NewTMLogger(&logs), log.AllowInfo())
		logger.Debug("filtered", "v", boundedValue{v})
		require.Zero(t, formatted.Load())

		logger.Info("kept", "v", boundedValue{v})
		require.Equal(t, int32(1), formatted.Load())
		require.Contains(t, logs.String(), "v=counted")
	})

	t.Run("the seen-tx Debug line gets the tx hash lazily", func(t *testing.T) {
		app := abcimock.NewClient(t)
		app.On("InsertTx", mock.Anything, mock.Anything).Return(&abci.ResponseInsertTx{Code: abci.CodeTypeOK}, nil).Once()

		cfg := config.TestConfig().Mempool
		r := NewAppReactor(cfg, newTestAppMempool(t, cfg, app), false)
		logger := &recordingLogger{Logger: log.NewNopLogger()}
		r.SetLogger(logger)
		receiveTxs(r, "seen-tx", "seen-tx")

		require.IsType(t, txHashValue(nil), logger.value("Tx already seen", "tx"))
	})
}

func TestLoggedValueCaps(t *testing.T) {
	long := strings.Repeat("x", 10*maxLoggedValueLen)

	t.Run("call sites use the cap", func(t *testing.T) {
		require.Less(t, len(reapPanicError{value: long}.Error()), 2*maxLoggedValueLen)

		stacks := newLogThrottle()
		require.True(t, stacks.claim(), "keep the stack out so only the value is measured")

		var logs lockedBuffer
		logRecoveredPanic(log.NewTMLogger(&logs), stacks, "panic", long)
		require.Less(t, logs.Len(), 2*maxLoggedValueLen)
		require.Contains(t, logs.String(), "...(truncated)")
	})

	t.Run("app errors are capped in logs and the CheckTx reply", func(t *testing.T) {
		var reaps atomic.Int32
		app := abcimock.NewClient(t)
		app.On("CheckTx", mock.Anything, mock.Anything).Return((*abci.ResponseCheckTx)(nil), fmt.Errorf("%s", long))
		app.On("ReapTxs", mock.Anything, mock.Anything).
			Return(func(context.Context, *abci.RequestReapTxs) (*abci.ResponseReapTxs, error) {
				reaps.Add(1)
				return nil, fmt.Errorf("%s", long)
			})

		var logs lockedBuffer
		cfg := config.TestMempoolConfig()
		cfg.ReapInterval = 5 * time.Millisecond
		m := newTestAppMempool(t, cfg, app, WithAMLogger(log.NewTMLogger(&logs)))

		res := checkTxResult(t, m, types.Tx("long-error-tx"))
		require.Less(t, len(res.Log), 2*maxLoggedValueLen)

		ctx, cancel := context.WithCancel(context.Background())
		ch := m.TxStream(ctx)
		require.Eventually(t, func() bool { return reaps.Load() >= 3 }, time.Second, 5*time.Millisecond)
		cancel()
		for range ch {
		}

		out := logs.String()
		require.Contains(t, out, "error checking tx")
		require.Contains(t, out, "error reaping txs")
		for _, line := range strings.Split(out, "\n") {
			require.Less(t, len(line), 2*maxLoggedValueLen, line)
		}
	})
}

// lockedBuffer lets tests read logs while a writing goroutine may still be running.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}
