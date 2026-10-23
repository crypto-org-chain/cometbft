package mempool

import (
	"github.com/go-kit/kit/metrics"
)

const (
	// MetricsSubsystem is a subsystem shared by all metrics exposed by this
	// package.
	MetricsSubsystem = "mempool"
)

//go:generate go run ../scripts/metricsgen -struct=Metrics

// Metrics contains metrics exposed by this package.
// see MetricsProvider for descriptions.
type Metrics struct {
	// Number of uncommitted transactions in the mempool.
	Size metrics.Gauge

	// Total size of the mempool in bytes.
	SizeBytes metrics.Gauge

	// Histogram of transaction sizes in bytes.
	TxSizeBytes metrics.Histogram `metrics_buckettype:"exp" metrics_bucketsizes:"1,3,7"`

	// FailedTxs defines the number of failed transactions. In CListMempool these
	// are transactions deemed invalid; in AppMempool, transactions the app failed
	// to process (client error, nil response or panic).
	// metrics:Number of failed transactions.
	FailedTxs metrics.Counter

	// RejectedTxs defines the number of rejected transactions. In CListMempool
	// these hit resource limits, e.g. mempool is full, or were already in the
	// mempool; in AppMempool, these are peer txs (InsertTx) the app rejected
	// with a non-retryable code.
	// metrics:Number of rejected transactions.
	RejectedTxs metrics.Counter

	// EvictedTxs defines the number of evicted transactions. These are valid
	// transactions that passed CheckTx and make it into the mempool but later
	// became invalid.
	// metrics:Number of evicted transactions.
	EvictedTxs metrics.Counter

	// Number of times transactions are rechecked in the mempool.
	RecheckTimes metrics.Counter

	// Number of connections being actively used for gossiping transactions
	// (experimental feature).
	ActiveOutboundConnections metrics.Gauge

	// Number of times transactions were received more than once.
	//metrics:Number of duplicate transaction reception.
	AlreadyReceivedTxs metrics.Counter

	// BatchSize size of an inbound/outbound batch of mempool txs (in txs num, not bytes)
	BatchSize metrics.Histogram `metrics_labels:"dir" metrics_bucketsizes:"1,2,5,10,30,50,100,200,300"`

	// ReapedTxs is the number of transactions reaped from the mempool
	ReapedTxs metrics.Counter
}
