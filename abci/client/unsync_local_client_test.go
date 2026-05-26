package abcicli_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	abcicli "github.com/cometbft/cometbft/abci/client"
	"github.com/cometbft/cometbft/abci/types"
)

// concurrentApp counts how many CheckTx calls observe a peer mid-flight.
// If the unsyncLocalClient is correctly lock-free, observed > 0 under
// concurrent load; if a global mutex is silently reintroduced, observed == 0
// because calls would serialize.
type concurrentApp struct {
	types.BaseApplication

	inFlight int32
	maxSeen  int32
}

func (a *concurrentApp) CheckTx(_ context.Context, _ *types.RequestCheckTx) (*types.ResponseCheckTx, error) {
	cur := atomic.AddInt32(&a.inFlight, 1)
	defer atomic.AddInt32(&a.inFlight, -1)
	for {
		prev := atomic.LoadInt32(&a.maxSeen)
		if cur <= prev || atomic.CompareAndSwapInt32(&a.maxSeen, prev, cur) {
			break
		}
	}
	return &types.ResponseCheckTx{Code: types.CodeTypeOK}, nil
}

func TestUnsyncLocalClient_ConcurrentCheckTxObservesParallelism(t *testing.T) {
	app := &concurrentApp{}
	cli := abcicli.NewUnsyncLocalClient(app)
	require.NoError(t, cli.Start())
	t.Cleanup(func() { _ = cli.Stop() })

	cli.SetResponseCallback(func(*types.Request, *types.Response) {})

	const goroutines = 32
	const perGoroutine = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				_, err := cli.CheckTxAsync(context.Background(), &types.RequestCheckTx{Tx: []byte("tx")})
				require.NoError(t, err)
			}
		}()
	}
	wg.Wait()

	// Lock-free client must let multiple CheckTx calls overlap.
	require.Greater(t, atomic.LoadInt32(&app.maxSeen), int32(1),
		"expected concurrent CheckTx calls to overlap; observed serialization (maxSeen=%d)", app.maxSeen)
}

func TestUnsyncLocalClient_CheckTxInvokesCallback(t *testing.T) {
	app := types.BaseApplication{}
	cli := abcicli.NewUnsyncLocalClient(app)
	require.NoError(t, cli.Start())
	t.Cleanup(func() { _ = cli.Stop() })

	var got atomic.Int32
	cli.SetResponseCallback(func(_ *types.Request, _ *types.Response) {
		got.Add(1)
	})

	_, err := cli.CheckTxAsync(context.Background(), &types.RequestCheckTx{Tx: []byte("tx")})
	require.NoError(t, err)
	require.Equal(t, int32(1), got.Load())
}

func TestUnsyncLocalClient_SetResponseCallbackRaceFree(t *testing.T) {
	app := types.BaseApplication{}
	cli := abcicli.NewUnsyncLocalClient(app)
	require.NoError(t, cli.Start())
	t.Cleanup(func() { _ = cli.Stop() })

	cb := func(*types.Request, *types.Response) {}
	cli.SetResponseCallback(cb)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			cli.SetResponseCallback(cb)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_, err := cli.CheckTxAsync(context.Background(), &types.RequestCheckTx{Tx: []byte("tx")})
			require.NoError(t, err)
		}
	}()
	wg.Wait()
}
