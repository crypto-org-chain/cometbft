package block_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	dbm "github.com/cometbft/cometbft-db"

	"github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/state/indexer/block"
	"github.com/cometbft/cometbft/state/txindex"
)

// rpc/core paginates before loading txs only when the configured indexer is a
// PageSearcher; otherwise it silently loads every match.
func TestKVTxIndexerIsPageSearcher(t *testing.T) {
	cfg := config.TestConfig()
	cfg.TxIndex.Indexer = "kv"
	memDBProvider := func(*config.DBContext) (dbm.DB, error) { return dbm.NewMemDB(), nil }

	txIdx, _, err := block.IndexerFromConfig(cfg, memDBProvider, "test-chain")
	require.NoError(t, err)
	require.Implements(t, (*txindex.PageSearcher)(nil), txIdx)
}
