package abcicli

import (
	"context"
	"sync/atomic"

	types "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/libs/service"
)

// unsyncLocalClient is a variant of localClient that does NOT acquire an
// application-wide mutex around ABCI calls. The wrapped Application must be
// safe for concurrent use; callers (e.g. cosmos-sdk + ethermint) are
// responsible for their own concurrency control.
//
// The response Callback is stored behind atomic.Pointer for lock-free reads
// on every CheckTxAsync. App invocations (CheckTx, Query, Commit,
// FinalizeBlock, ...) reach the wrapped Application directly via embedding
// promotion, with no lock.
type unsyncLocalClient struct {
	service.BaseService

	types.Application
	cb atomic.Pointer[Callback]
}

var _ Client = (*unsyncLocalClient)(nil)

// NewUnsyncLocalClient creates a local client that does not synchronize ABCI
// calls behind a global mutex. The wrapped Application is expected to be
// concurrency-safe.
func NewUnsyncLocalClient(app types.Application) Client {
	cli := &unsyncLocalClient{
		Application: app,
	}
	cli.BaseService = *service.NewBaseService(nil, "unsyncLocalClient", cli)
	return cli
}

func (app *unsyncLocalClient) SetResponseCallback(cb Callback) {
	app.cb.Store(&cb)
}

func (app *unsyncLocalClient) CheckTxAsync(ctx context.Context, req *types.RequestCheckTx) (*ReqRes, error) {
	res, err := app.Application.CheckTx(ctx, req)
	if err != nil {
		return nil, err
	}
	return app.callback(
		types.ToRequestCheckTx(req),
		types.ToResponseCheckTx(res),
	), nil
}

func (app *unsyncLocalClient) callback(req *types.Request, res *types.Response) *ReqRes {
	(*app.cb.Load())(req, res)
	rr := newLocalReqRes(req, res)
	rr.callbackInvoked = true
	return rr
}

// Client interface methods not provided by types.Application.

func (app *unsyncLocalClient) Error() error { return nil }

func (app *unsyncLocalClient) Flush(context.Context) error { return nil }

func (app *unsyncLocalClient) Echo(_ context.Context, msg string) (*types.ResponseEcho, error) {
	return &types.ResponseEcho{Message: msg}, nil
}
