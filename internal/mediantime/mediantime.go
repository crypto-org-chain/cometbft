// Package mediantime holds the per-chain heights from which block time skips
// Nil precommits (cometbft#5901). It is internal so no importer of this module
// can change a consensus rule at runtime.
package mediantime

import (
	"maps"
	"sync"
	"sync/atomic"
)

// releaseHeights maps a chain-id to its activation height. Below it, Nil
// precommits count as in v0.39, so blocks committed by v0.39 binaries still
// replay. Set each entry to the chain's upgrade height + 1 at release time:
// every validator must run the same value, or the chain splits at the first
// block whose last commit has a Nil precommit that moves the median. Chains
// not listed always count Nil precommits.
var releaseHeights = map[string]int64{
	"cronosmainnet_25-1":  0,
	"cronostestnet_338-3": 0,
}

var (
	// Read on every block; replaced copy-on-write only by tests, while earlier
	// tests' nodes may still be reading it.
	heights  atomic.Pointer[map[string]int64]
	writeMtx sync.Mutex
)

func init() {
	heights.Store(&releaseHeights)
}

// ActivationHeight returns chainID's activation height. ok is false for chains
// that always count Nil precommits.
func ActivationHeight(chainID string) (height int64, ok bool) {
	height, ok = (*heights.Load())[chainID]
	return height, ok
}

// SetForTesting sets chainID's activation height, known chains included, and
// returns a func that restores the previous value.
func SetForTesting(chainID string, height int64) (restore func()) {
	prev, existed := ActivationHeight(chainID)
	update(func(h map[string]int64) { h[chainID] = height })
	return func() {
		update(func(h map[string]int64) {
			if existed {
				h[chainID] = prev
			} else {
				delete(h, chainID)
			}
		})
	}
}

func update(apply func(map[string]int64)) {
	writeMtx.Lock()
	defer writeMtx.Unlock()
	next := maps.Clone(*heights.Load())
	apply(next)
	heights.Store(&next)
}
