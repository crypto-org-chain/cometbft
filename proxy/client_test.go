package proxy

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/cometbft/cometbft/abci/types"
)

// TestNewConsensusSyncLocalClientCreator_ConnTypeSplit asserts that the
// creator hands a locking client to the consensus connection and lock-free
// clients to mempool / query / snapshot.
//
// The contract is structural — an unsynced mempool client is the whole point
// of Patch 3. If the consensus connection ever stops getting a locking
// client (or vice versa), this test fails.
func TestNewConsensusSyncLocalClientCreator_ConnTypeSplit(t *testing.T) {
	app := types.NewBaseApplication()
	cc := NewConsensusSyncLocalClientCreator(app)

	consensus, err := cc.NewABCIConsensusClient()
	require.NoError(t, err)
	require.Contains(t, fmt.Sprintf("%T", consensus), "localClient",
		"consensus conn must use the locking localClient (got %T)", consensus)
	require.NotContains(t, fmt.Sprintf("%T", consensus), "unsync",
		"consensus conn must NOT use unsyncLocalClient (got %T)", consensus)

	for _, tc := range []struct {
		name string
		make func() (any, error)
	}{
		{"mempool", func() (any, error) { return cc.NewABCIMempoolClient() }},
		{"query", func() (any, error) { return cc.NewABCIQueryClient() }},
		{"snapshot", func() (any, error) { return cc.NewABCISnapshotClient() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli, err := tc.make()
			require.NoError(t, err)
			require.Contains(t, fmt.Sprintf("%T", cli), "unsyncLocalClient",
				"%s conn must use unsyncLocalClient (got %T)", tc.name, cli)
		})
	}
}

func TestNewUnsyncLocalClientCreator_AllUnsync(t *testing.T) {
	app := types.NewBaseApplication()
	cc := NewUnsyncLocalClientCreator(app)

	for _, tc := range []struct {
		name string
		make func() (any, error)
	}{
		{"consensus", func() (any, error) { return cc.NewABCIConsensusClient() }},
		{"mempool", func() (any, error) { return cc.NewABCIMempoolClient() }},
		{"query", func() (any, error) { return cc.NewABCIQueryClient() }},
		{"snapshot", func() (any, error) { return cc.NewABCISnapshotClient() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli, err := tc.make()
			require.NoError(t, err)
			require.Contains(t, fmt.Sprintf("%T", cli), "unsyncLocalClient",
				"%s conn must use unsyncLocalClient (got %T)", tc.name, cli)
		})
	}
}
