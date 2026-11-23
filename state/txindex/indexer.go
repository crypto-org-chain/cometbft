package txindex

import (
	"context"
	"errors"

	"github.com/cometbft/cometbft/libs/log"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/libs/pubsub/query"
)

// XXX/TODO: These types should be moved to the indexer package.

//go:generate ../../scripts/mockery_generate.sh TxIndexer

// TxIndexer interface defines methods to index and search transactions.
type TxIndexer interface {
	// AddBatch analyzes, indexes and stores a batch of transactions.
	AddBatch(b *Batch) error

	// Index analyzes, indexes and stores a single transaction.
	Index(result *abci.TxResult) error

	// Get returns the transaction specified by hash or nil if the transaction is not indexed
	// or stored.
	Get(hash []byte) (*abci.TxResult, error)

	// Search allows you to query for transactions.
	Search(ctx context.Context, q *query.Query) ([]*abci.TxResult, error)

	// Set Logger
	SetLogger(l log.Logger)
}

// Batch groups together multiple Index operations to be performed at the same time.
// NOTE: Batch is NOT thread-safe and must not be modified after starting its execution.
type Batch struct {
	Ops []*abci.TxResult
}

// PageSearcher is implemented by indexers that can order and paginate matches
// before loading them, so a page costs less than loading every match.
type PageSearcher interface {
	// SearchPage returns one page of the transactions matching q, ordered by
	// height and index, and the total number of matches.
	SearchPage(ctx context.Context, q *query.Query, pagSettings Pagination) ([]*abci.TxResult, int, error)
}

// Pagination selects one page of search results. A PerPage of 0 returns all
// matches.
type Pagination struct {
	OrderDesc bool
	Page      int
	PerPage   int
}

// Paginate returns the requested page of s, or nil when page is out of range;
// callers validate page against len(s). A perPage of 0 returns all of s,
// whatever the page.
func Paginate[T any](s []T, page, perPage int) []T {
	if perPage == 0 {
		return s
	}
	// page comes from the request; checking it before multiplying keeps
	// (page-1)*perPage from overflowing.
	if page < 1 || perPage < 1 || page-1 > len(s)/perPage {
		return nil
	}
	start := (page - 1) * perPage
	if start >= len(s) {
		return nil
	}
	end := start + perPage
	if end > len(s) {
		end = len(s)
	}
	return s[start:end]
}

// NewBatch creates a new Batch.
func NewBatch(n int64) *Batch {
	return &Batch{
		Ops: make([]*abci.TxResult, n),
	}
}

// Add or update an entry for the given result.Index.
func (b *Batch) Add(result *abci.TxResult) error {
	b.Ops[result.Index] = result
	return nil
}

// Size returns the total number of operations inside the batch.
func (b *Batch) Size() int {
	return len(b.Ops)
}

// ErrorEmptyHash indicates empty hash
var ErrorEmptyHash = errors.New("transaction hash cannot be empty")
