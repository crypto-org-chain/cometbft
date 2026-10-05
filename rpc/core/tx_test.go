package core

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	db "github.com/cometbft/cometbft-db"

	abci "github.com/cometbft/cometbft/abci/types"
	rpctypes "github.com/cometbft/cometbft/rpc/jsonrpc/types"
	"github.com/cometbft/cometbft/state/txindex"
	"github.com/cometbft/cometbft/state/txindex/kv"
	"github.com/cometbft/cometbft/types"
)

// searchOnlyIndexer hides SearchPage so TxSearch takes the fallback path.
type searchOnlyIndexer struct {
	txindex.TxIndexer
}

func TestTxSearchPagination(t *testing.T) {
	indexer := kv.NewTxIndex(db.NewMemDB())
	// Indexed out of order to check that both paths sort by height, then index.
	positions := [][2]int64{{2, 1}, {1, 0}, {3, 0}, {2, 0}, {1, 1}}
	for _, pos := range positions {
		require.NoError(t, indexer.Index(&abci.TxResult{
			Height: pos[0],
			Index:  uint32(pos[1]),
			Tx:     types.Tx(fmt.Sprintf("tx-%d-%d", pos[0], pos[1])),
			Result: abci.ExecTxResult{Events: []abci.Event{
				{Type: "account", Attributes: []abci.EventAttribute{{Key: "owner", Value: "Ivan", Index: true}}},
			}},
		}))
	}

	testCases := []struct {
		name     string
		page     int
		perPage  int
		orderBy  string
		expected [][2]int64
		expErr   bool
	}{
		{"first page ascending", 1, 2, "asc", [][2]int64{{1, 0}, {1, 1}}, false},
		{"last partial page ascending", 3, 2, "", [][2]int64{{3, 0}}, false},
		{"first page descending", 1, 3, "desc", [][2]int64{{3, 0}, {2, 1}, {2, 0}}, false},
		{"page out of range", 4, 2, "asc", nil, true},
	}

	indexers := map[string]txindex.TxIndexer{
		"page searcher": indexer,
		"fallback":      searchOnlyIndexer{indexer},
	}

	for indexerName, txIndexer := range indexers {
		env := &Environment{TxIndexer: txIndexer}
		for _, tc := range testCases {
			t.Run(indexerName+"/"+tc.name, func(t *testing.T) {
				res, err := env.TxSearch(&rpctypes.Context{}, "account.owner = 'Ivan'", false, &tc.page, &tc.perPage, tc.orderBy)
				if tc.expErr {
					require.Error(t, err)
					return
				}
				require.NoError(t, err)
				require.Equal(t, len(positions), res.TotalCount)

				got := make([][2]int64, 0, len(res.Txs))
				for _, tx := range res.Txs {
					got = append(got, [2]int64{tx.Height, int64(tx.Index)})
				}
				require.Equal(t, tc.expected, got)
			})
		}
	}
}
