package mediantime

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A missing or misspelled entry would silently keep the v0.39 rule forever.
func TestReleaseHeightsListKnownChains(t *testing.T) {
	for _, chainID := range []string{"cronosmainnet_25-1", "cronostestnet_338-3"} {
		_, ok := ActivationHeight(chainID)
		require.True(t, ok, chainID)
	}
}

func TestSetForTestingRestore(t *testing.T) {
	const chainID = "median_time_restore_chain"

	testCases := []struct {
		name     string
		malleate func(t *testing.T)
		expOK    bool
		expAfter int64
	}{
		{"new entry is deleted", func(*testing.T) {}, false, 0},
		{"existing entry is reverted", func(t *testing.T) {
			t.Cleanup(SetForTesting(chainID, 7))
		}, true, 7},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.malleate(t)

			restore := SetForTesting(chainID, 100)
			height, ok := ActivationHeight(chainID)
			require.True(t, ok)
			require.EqualValues(t, 100, height)

			restore()
			height, ok = ActivationHeight(chainID)
			require.Equal(t, tc.expOK, ok)
			require.Equal(t, tc.expAfter, height)
		})
	}
}
