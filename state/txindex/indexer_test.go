package txindex_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/cometbft/cometbft/state/txindex"
)

func TestPaginate(t *testing.T) {
	s := []int{1, 2, 3, 4, 5}

	testCases := []struct {
		name     string
		page     int
		perPage  int
		expected []int
	}{
		{"first page", 1, 2, []int{1, 2}},
		{"last partial page", 3, 2, []int{5}},
		{"page out of range", 4, 2, nil},
		{"page zero", 0, 2, nil},
		{"negative per page", 1, -1, nil},
		{"zero per page returns all", 7, 0, s},
		{"page overflow", math.MaxInt, 2, nil},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, txindex.Paginate(s, tc.page, tc.perPage))
		})
	}
}
