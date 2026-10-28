package e2e_test

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cometbft/cometbft/lp2p"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	e2e "github.com/cometbft/cometbft/test/e2e/pkg"
)

// Tests that all nodes have peered with each other, regardless of discovery method.
func TestNet_Peers(t *testing.T) {
	// FIXME Skip test since nodes aren't always able to fully mesh
	t.SkipNow()

	testNode(t, func(t *testing.T, node e2e.Node) {
		// Seed nodes shouldn't necessarily mesh with the entire network.
		if node.Mode == e2e.ModeSeed {
			return
		}

		client, err := node.Client()
		require.NoError(t, err)
		netInfo, err := client.NetInfo(ctx)
		require.NoError(t, err)

		require.Equal(t, len(node.Testnet.Nodes)-1, netInfo.NPeers,
			"node is not fully meshed with peers")

		seen := map[string]bool{}
		for _, n := range node.Testnet.Nodes {
			seen[n.Name] = (n.Name == node.Name) // we've clearly seen ourself
		}
		for _, peerInfo := range netInfo.Peers {
			peer := node.Testnet.LookupNode(peerInfo.NodeInfo.Moniker)
			require.NotNil(t, peer, "unknown node %v", peerInfo.NodeInfo.Moniker)
			require.Equal(t, peer.InternalIP.String(), peerInfo.RemoteIP,
				"unexpected IP address for peer %v", peer.Name)
			seen[peerInfo.NodeInfo.Moniker] = true
		}

		for name := range seen {
			require.True(t, seen[name], "node %v not peered with %v", node.Name, name)
		}
	})
}

// Peer exchange nodes must reach a peer that the manifest does not link them to.
func TestNet_PEXDiscoversPeers(t *testing.T) {
	if !slices.ContainsFunc(loadTestnet(t).Nodes, func(n *e2e.Node) bool { return n.Libp2pPEX }) {
		t.Skip("no peer exchange nodes")
	}

	testNode(t, func(t *testing.T, node e2e.Node) {
		if !node.Libp2pPEX {
			return
		}

		linked := map[string]bool{node.Name: true}
		for _, other := range node.Testnet.Nodes {
			for _, peer := range slices.Concat(other.Seeds, other.PersistentPeers) {
				switch node.Name {
				case other.Name:
					linked[peer.Name] = true
				case peer.Name:
					linked[other.Name] = true
				}
			}
		}
		if len(linked) == len(node.Testnet.Nodes) {
			return
		}

		names := make(map[string]string, len(node.Testnet.Nodes))
		for _, n := range node.Testnet.Nodes {
			id, err := lp2p.IDFromPrivateKey(n.NodeKey)
			require.NoError(t, err)
			names[id.String()] = n.Name
		}

		client, err := node.Client()
		require.NoError(t, err)
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			netInfo, err := client.NetInfo(ctx)
			if !assert.NoError(c, err) {
				return
			}
			assert.True(c, slices.ContainsFunc(netInfo.Peers, func(p ctypes.Peer) bool {
				name, ok := names[string(p.NodeInfo.ID())]
				return ok && !linked[name]
			}), "%s found no peer beyond the ones linked to it", node.Name)
		}, 4*time.Minute, time.Second)
	})
}
