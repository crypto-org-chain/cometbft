package core

import (
	"context"
	"errors"
	"fmt"
	"sort"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtquery "github.com/cometbft/cometbft/libs/pubsub/query"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	rpctypes "github.com/cometbft/cometbft/rpc/jsonrpc/types"
	"github.com/cometbft/cometbft/state/txindex"
	"github.com/cometbft/cometbft/state/txindex/null"
	"github.com/cometbft/cometbft/types"
)

// Tx allows you to query the transaction results. `nil` could mean the
// transaction is in the mempool, invalidated, or was not sent in the first
// place.
// More: https://docs.cometbft.com/v0.38.x/rpc/#/Info/tx
func (env *Environment) Tx(_ *rpctypes.Context, hash []byte, prove bool) (*ctypes.ResultTx, error) {
	// if index is disabled, return error
	if _, ok := env.TxIndexer.(*null.TxIndex); ok {
		return nil, fmt.Errorf("transaction indexing is disabled")
	}

	r, err := env.TxIndexer.Get(hash)
	if err != nil {
		return nil, err
	}

	if r == nil {
		return nil, fmt.Errorf("tx (%X) not found", hash)
	}

	var proof types.TxProof
	if prove {
		block := env.BlockStore.LoadBlock(r.Height)
		if block != nil {
			proof = block.Txs.Proof(int(r.Index))
		}
	}

	return &ctypes.ResultTx{
		Hash:     hash,
		Height:   r.Height,
		Index:    r.Index,
		TxResult: r.Result,
		Tx:       r.Tx,
		Proof:    proof,
	}, nil
}

// TxSearch allows you to query for multiple transactions results. It returns a
// list of transactions (maximum ?per_page entries) and the total count.
// More: https://docs.cometbft.com/v0.38.x/rpc/#/Info/tx_search
func (env *Environment) TxSearch(
	ctx *rpctypes.Context,
	query string,
	prove bool,
	pagePtr, perPagePtr *int,
	orderBy string,
) (*ctypes.ResultTxSearch, error) {
	// if index is disabled, return error
	if _, ok := env.TxIndexer.(*null.TxIndex); ok {
		return nil, errors.New("transaction indexing is disabled")
	} else if len(query) > maxQueryLength {
		return nil, errors.New("maximum query length exceeded")
	}

	var orderDesc bool
	switch orderBy {
	case "desc":
		orderDesc = true
	case "asc", "":
	default:
		return nil, errors.New("expected order_by to be either `asc` or `desc` or empty")
	}

	q, err := cmtquery.New(query)
	if err != nil {
		return nil, err
	}

	perPage := env.validatePerPage(perPagePtr)
	page := 1
	if pagePtr != nil {
		page = *pagePtr
	}

	results, totalCount, err := env.searchTxs(ctx.Context(), q, txindex.Pagination{
		OrderDesc:   orderDesc,
		IsPaginated: true,
		Page:        page,
		PerPage:     perPage,
	})
	if err != nil {
		return nil, err
	}

	if _, err := validatePage(pagePtr, perPage, totalCount); err != nil {
		return nil, err
	}

	apiResults := make([]*ctypes.ResultTx, 0, len(results))
	for _, r := range results {
		var proof types.TxProof
		if prove {
			block := env.BlockStore.LoadBlock(r.Height)
			if block != nil {
				proof = block.Txs.Proof(int(r.Index))
			}
		}

		apiResults = append(apiResults, &ctypes.ResultTx{
			Hash:     types.Tx(r.Tx).Hash(),
			Height:   r.Height,
			Index:    r.Index,
			TxResult: r.Result,
			Tx:       r.Tx,
			Proof:    proof,
		})
	}

	return &ctypes.ResultTxSearch{Txs: apiResults, TotalCount: totalCount}, nil
}

// searchTxs falls back to loading every match when the indexer cannot
// paginate on its own.
func (env *Environment) searchTxs(
	ctx context.Context,
	q *cmtquery.Query,
	pagSettings txindex.Pagination,
) ([]*abci.TxResult, int, error) {
	if ps, ok := env.TxIndexer.(txindex.PageSearcher); ok {
		return ps.SearchPage(ctx, q, pagSettings)
	}

	results, err := env.TxIndexer.Search(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	sort.Slice(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if pagSettings.OrderDesc {
			a, b = b, a
		}
		if a.Height == b.Height {
			return a.Index < b.Index
		}
		return a.Height < b.Height
	})
	return txindex.Paginate(results, pagSettings.Page, pagSettings.PerPage), len(results), nil
}
