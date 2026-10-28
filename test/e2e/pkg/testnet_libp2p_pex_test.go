package e2e_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	e2e "github.com/cometbft/cometbft/test/e2e/pkg"
)

func TestLibp2pPEXNetwork(t *testing.T) {
	manifestPath, err := filepath.Abs(filepath.Join("..", "networks", "libp2p-pex.toml"))
	require.NoError(t, err)

	testnet, err := loadTestnet(manifestPath)
	require.NoError(t, err)

	seeds := map[string]string{
		"validator01": "full01",
		"validator02": "validator01",
		"validator03": "validator01",
		"validator04": "validator01",
		"full01":      "validator01",
	}
	require.Len(t, testnet.Nodes, len(seeds))
	for _, node := range testnet.Nodes {
		require.True(t, node.UseLibp2p, node.Name)
		require.Equal(t, node.Name != "full01", node.Libp2pPEX, node.Name)
		require.Empty(t, node.PersistentPeers, node.Name)
		require.Len(t, node.Seeds, 1, node.Name)
		require.Equal(t, seeds[node.Name], node.Seeds[0].Name, node.Name)
	}
}

func TestLibp2pPEXRequiresLibp2p(t *testing.T) {
	manifestPath := filepath.Join(t.TempDir(), "manifest.toml")
	require.NoError(t, os.WriteFile(manifestPath, []byte("[node.validator01]\nlibp2p_pex = true\n"), 0o600))

	_, err := loadTestnet(manifestPath)
	require.ErrorContains(t, err, "libp2p_pex requires use_libp2p")
}

func loadTestnet(manifestPath string) (*e2e.Testnet, error) {
	manifest, err := e2e.LoadManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	ifd, err := e2e.NewDockerInfrastructureData(manifest)
	if err != nil {
		return nil, err
	}

	return e2e.LoadTestnet(manifestPath, ifd)
}
