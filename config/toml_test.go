package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/internal/test"
)

func ensureFiles(t *testing.T, rootDir string, files ...string) {
	for _, f := range files {
		p := filepath.Join(rootDir, f)
		_, err := os.Stat(p)
		assert.NoError(t, err, p)
	}
}

func TestEnsureRoot(t *testing.T) {
	require := require.New(t)

	// setup temp dir for test
	tmpDir, err := os.MkdirTemp("", "config-test")
	require.Nil(err)
	defer os.RemoveAll(tmpDir)

	// create root dir
	config.EnsureRoot(tmpDir)

	// make sure config is set properly
	data, err := os.ReadFile(filepath.Join(tmpDir, config.DefaultConfigDir, config.DefaultConfigFileName))
	require.Nil(err)

	assertValidConfig(t, string(data))

	ensureFiles(t, tmpDir, "data")
}

func TestEnsureTestRoot(t *testing.T) {
	require := require.New(t)

	// create root dir
	cfg := test.ResetTestRoot("ensureTestRoot")
	defer os.RemoveAll(cfg.RootDir)
	rootDir := cfg.RootDir

	// make sure config is set properly
	data, err := os.ReadFile(filepath.Join(rootDir, config.DefaultConfigDir, config.DefaultConfigFileName))
	require.Nil(err)

	assertValidConfig(t, string(data))

	// TODO: make sure the cfg returned and testconfig are the same!
	baseConfig := config.DefaultBaseConfig()
	ensureFiles(t, rootDir, config.DefaultDataDir, baseConfig.Genesis, baseConfig.PrivValidatorKey, baseConfig.PrivValidatorState)
}

func assertValidConfig(t *testing.T, configFile string) {
	t.Helper()
	// list of words we expect in the config
	elems := []string{
		"moniker",
		"seeds",
		"proxy_app",
		"create_empty_blocks",
		"peer",
		"timeout",
		"broadcast",
		"send",
		"addr",
		"wal",
		"propose",
		"max",
		"genesis",
	}
	for _, e := range elems {
		assert.Contains(t, configFile, e)
	}
}

func TestLibP2PConfigRoundTrip(t *testing.T) {
	custom := config.DefaultConfig()
	custom.P2P.LibP2PConfig.PrivatePeerIDs = []string{"peerA", "peerB"}
	custom.P2P.LibP2PConfig.PEX = config.LibP2PPEX{Enabled: true, AddrBookFile: `C:\data\addr book.json`}

	defaults := config.DefaultLibP2PConfig()

	testCases := []struct {
		name     string
		write    func(path string)
		expected config.LibP2PConfig
	}{
		{
			name:     "custom values",
			write:    func(path string) { config.WriteConfigFile(path, custom) },
			expected: custom.P2P.LibP2PConfig,
		},
		{
			name: "file without the new keys",
			write: func(path string) {
				require.NoError(t, os.WriteFile(path, []byte("[p2p.libp2p]\nenabled = false\n"), 0o600))
			},
			expected: defaults,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), config.DefaultConfigFileName)
			tc.write(path)

			v := viper.New()
			v.SetConfigFile(path)
			require.NoError(t, v.ReadInConfig())

			actual := config.DefaultConfig()
			require.NoError(t, v.Unmarshal(actual))

			assert.Equal(t, tc.expected.PrivatePeerIDs, actual.P2P.LibP2PConfig.PrivatePeerIDs)
			assert.Equal(t, tc.expected.PEX, actual.P2P.LibP2PConfig.PEX)
		})
	}
}
