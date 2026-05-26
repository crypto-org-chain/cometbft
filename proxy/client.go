package proxy

import (
	"fmt"

	abcicli "github.com/cometbft/cometbft/abci/client"
	"github.com/cometbft/cometbft/abci/example/kvstore"
	"github.com/cometbft/cometbft/abci/types"
	cmtsync "github.com/cometbft/cometbft/libs/sync"
	e2e "github.com/cometbft/cometbft/test/e2e/app"
)

//go:generate ../scripts/mockery_generate.sh ClientCreator

// ClientCreator creates new ABCI clients, one per CometBFT-to-application
// connection type (consensus, mempool, query, snapshot). Splitting client
// construction by connection type lets a creator hand back per-conn clients
// with different concurrency models — e.g. a locking client for the
// consensus connection alongside lock-free clients for mempool/query/snapshot.
type ClientCreator interface {
	// NewABCIConsensusClient returns the ABCI client used for the consensus
	// connection (Commit, FinalizeBlock, ...).
	NewABCIConsensusClient() (abcicli.Client, error)
	// NewABCIMempoolClient returns the ABCI client used for the mempool
	// connection (CheckTx, ...).
	NewABCIMempoolClient() (abcicli.Client, error)
	// NewABCIQueryClient returns the ABCI client used for the query
	// connection (Query, Info, ...).
	NewABCIQueryClient() (abcicli.Client, error)
	// NewABCISnapshotClient returns the ABCI client used for the state-sync
	// snapshot connection.
	NewABCISnapshotClient() (abcicli.Client, error)
}

// uniformClientCreator implements ClientCreator by routing every per-conn
// method to a single factory function. Embed it (and assign make in the
// constructor) when all four connections should produce identical clients.
type uniformClientCreator struct {
	make func() (abcicli.Client, error)
}

func (u *uniformClientCreator) NewABCIConsensusClient() (abcicli.Client, error) {
	return u.make()
}
func (u *uniformClientCreator) NewABCIMempoolClient() (abcicli.Client, error) {
	return u.make()
}
func (u *uniformClientCreator) NewABCIQueryClient() (abcicli.Client, error) {
	return u.make()
}
func (u *uniformClientCreator) NewABCISnapshotClient() (abcicli.Client, error) {
	return u.make()
}

//----------------------------------------------------
// local proxy uses a single mutex on an in-proc app

// NewLocalClientCreator returns a [ClientCreator] for the given app, which
// will be running locally.
//
// All four per-conn clients share a single mutex, serializing every ABCI call
// across connections. For per-connection mutexes, see
// [NewConnSyncLocalClientCreator].
func NewLocalClientCreator(app types.Application) ClientCreator {
	mtx := new(cmtsync.Mutex)
	return &uniformClientCreator{
		make: func() (abcicli.Client, error) {
			return abcicli.NewLocalClient(mtx, app), nil
		},
	}
}

//----------------------------------------------------
// local proxy creates a new mutex for each client

// NewConnSyncLocalClientCreator returns a local [ClientCreator] for the given
// app.
//
// Unlike [NewLocalClientCreator], this is a "connection-synchronized" local
// client creator: each per-conn client maintains its own mutex over the
// application, so calls on one connection do not block calls on another.
func NewConnSyncLocalClientCreator(app types.Application) ClientCreator {
	return &uniformClientCreator{
		make: func() (abcicli.Client, error) {
			// nil mtx => each instance creates its own.
			return abcicli.NewLocalClient(nil, app), nil
		},
	}
}

//----------------------------------------------------
// fully unsynced local creator

// NewUnsyncLocalClientCreator returns a local [ClientCreator] that uses
// [abcicli.NewUnsyncLocalClient] for all four connections. The application
// must be fully concurrency-safe; no mutex is held on any ABCI call.
func NewUnsyncLocalClientCreator(app types.Application) ClientCreator {
	return &uniformClientCreator{
		make: func() (abcicli.Client, error) {
			return abcicli.NewUnsyncLocalClient(app), nil
		},
	}
}

//----------------------------------------------------
// consensus-sync local creator: locking on consensus, lock-free elsewhere

// consensusSyncLocalClientCreator embeds uniformClientCreator for
// mempool/query/snapshot and overrides NewABCIConsensusClient with a
// dedicated factory for the consensus conn.
type consensusSyncLocalClientCreator struct {
	uniformClientCreator // mempool / query / snapshot
	makeConsensus        func() (abcicli.Client, error)
}

func (c *consensusSyncLocalClientCreator) NewABCIConsensusClient() (abcicli.Client, error) {
	return c.makeConsensus()
}

// NewConsensusSyncLocalClientCreator returns a local [ClientCreator] that
// gives the consensus connection a locking [abcicli.NewLocalClient] (with its
// own mutex) and gives mempool/query/snapshot connections a lock-free
// [abcicli.NewUnsyncLocalClient]. The application must be safe for concurrent
// use across the lock-free connections.
func NewConsensusSyncLocalClientCreator(app types.Application) ClientCreator {
	mtx := new(cmtsync.Mutex)
	return &consensusSyncLocalClientCreator{
		uniformClientCreator: uniformClientCreator{
			make: func() (abcicli.Client, error) {
				return abcicli.NewUnsyncLocalClient(app), nil
			},
		},
		makeConsensus: func() (abcicli.Client, error) {
			return abcicli.NewLocalClient(mtx, app), nil
		},
	}
}

//---------------------------------------------------------------
// remote proxy opens new connections to an external app process

// NewRemoteClientCreator returns a ClientCreator for the given address (e.g.
// "192.168.0.1") and transport (e.g. "tcp"). Set mustConnect to true if you
// want the client to connect before reporting success.
func NewRemoteClientCreator(addr, transport string, mustConnect bool) ClientCreator {
	return &uniformClientCreator{
		make: func() (abcicli.Client, error) {
			remoteApp, err := abcicli.NewClient(addr, transport, mustConnect)
			if err != nil {
				return nil, fmt.Errorf("failed to connect to proxy: %w", err)
			}
			return remoteApp, nil
		},
	}
}

// DefaultClientCreator returns a default [ClientCreator], which will create a
// local client if addr is one of "kvstore", "persistent_kvstore", "e2e",
// "noop".
//
// Otherwise a remote client will be created.
//
// Each of "kvstore", "persistent_kvstore" and "e2e" also currently have an
// "_connsync" variant (i.e. "kvstore_connsync", etc.), which attempts to
// replicate the same concurrency model as the remote client.
func DefaultClientCreator(addr, transport, dbDir string) ClientCreator {
	switch addr {
	case "kvstore":
		return NewLocalClientCreator(kvstore.NewInMemoryApplication())
	case "kvstore_connsync":
		return NewConnSyncLocalClientCreator(kvstore.NewInMemoryApplication())
	case "persistent_kvstore":
		return NewLocalClientCreator(kvstore.NewPersistentApplication(dbDir))
	case "persistent_kvstore_connsync":
		return NewConnSyncLocalClientCreator(kvstore.NewPersistentApplication(dbDir))
	case "e2e":
		app, err := e2e.NewApplication(e2e.DefaultConfig(dbDir))
		if err != nil {
			panic(err)
		}
		return NewLocalClientCreator(app)
	case "e2e_connsync":
		app, err := e2e.NewApplication(e2e.DefaultConfig(dbDir))
		if err != nil {
			panic(err)
		}
		return NewConnSyncLocalClientCreator(app)
	case "noop":
		return NewLocalClientCreator(types.NewBaseApplication())
	default:
		mustConnect := false // loop retrying
		return NewRemoteClientCreator(addr, transport, mustConnect)
	}
}
