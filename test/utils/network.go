package utils

import (
	"io"
	"net"
	"testing"
)

// GetFreePorts returns n ports that are free for both TCP and UDP, since QUIC
// listeners bind UDP.
func GetFreePorts(t *testing.T, n int) []int {
	var (
		ports   = make([]int, 0, n)
		holders = make([]io.Closer, 0, 2*n)
	)

	for len(ports) < n {
		addr, err := net.ResolveTCPAddr("tcp", "localhost:0")
		if err != nil {
			t.Fatalf("unable to resolve port: %v", err)
		}

		l, err := net.ListenTCP("tcp", addr)
		if err != nil {
			t.Fatalf("unable to listen to tcp: %v", err)
		}
		holders = append(holders, l)

		port := l.Addr().(*net.TCPAddr).Port
		pc, err := net.ListenPacket("udp", l.Addr().String())
		if err != nil {
			continue
		}
		holders = append(holders, pc)

		ports = append(ports, port)
	}

	// keep every port held until all are chosen to avoid duplicates
	for _, h := range holders {
		if err := h.Close(); err != nil {
			t.Fatalf("unable to close listener: %v", err)
		}
	}

	return ports
}
