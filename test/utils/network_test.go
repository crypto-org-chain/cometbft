package utils

import (
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetFreePorts(t *testing.T) {
	ports := GetFreePorts(t, 5)

	seen := map[int]bool{}
	for _, port := range ports {
		require.False(t, seen[port], "duplicate port %d", port)
		seen[port] = true

		addr := fmt.Sprintf("localhost:%d", port)
		l, err := net.Listen("tcp", addr)
		require.NoError(t, err)
		pc, err := net.ListenPacket("udp", l.Addr().String())
		require.NoError(t, err)
		require.NoError(t, pc.Close())
		require.NoError(t, l.Close())
	}
}
