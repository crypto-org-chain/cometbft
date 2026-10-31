// Package mediantime holds per-chain heights from which block time skips Nil
// precommits (cometbft#5901). Internal so importers can't change consensus at runtime.
package mediantime

import (
	"maps"
	"sync"
	"sync/atomic"
)

// At release set each to upgrade height + 1, so the upgrade block v0.39
// committed still replays; 0 is unset and blocks startup. Every validator
// must run the same value or the chain splits.
var releaseHeights = map[string]int64{
	"cronosmainnet_25-1":  0,
	"cronostestnet_338-3": 0,
}

var (
	// Copy-on-write: tests swap it while earlier tests' nodes may still read it.
	heights  atomic.Pointer[map[string]int64]
	writeMtx sync.Mutex
)

func init() {
	heights.Store(&releaseHeights)
}

// ActivationHeight reports ok false for chains that always count Nil precommits.
func ActivationHeight(chainID string) (height int64, ok bool) {
	height, ok = (*heights.Load())[chainID]
	return height, ok
}

// SetForTesting overrides chainID's height, known chains included, until the
// returned func runs.
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
