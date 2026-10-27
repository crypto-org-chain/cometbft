package lp2p

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"testing"
	"testing/iotest"
	"time"

	"github.com/cometbft/cometbft/test/utils"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

func TestStream(t *testing.T) {
	t.Run("StreamWrite", func(t *testing.T) {
		t.Run("Write", func(t *testing.T) {
			// ARRANGE
			suite := newStreamTestSuite(t)

			payload := []byte("hello stream")
			actualPayload := make(chan []byte, 1)
			readErr := make(chan error, 1)

			suite.hostA.SetStreamHandler(suite.protoID, func(stream network.Stream) {
				bz, err := StreamReadClose(stream)
				if err != nil {
					readErr <- err
					return
				}

				actualPayload <- bz
				readErr <- nil
			})

			stream := suite.newStream(t)

			expectedFrame := append(uint64ToUvarint(uint64(len(payload))), payload...)

			// ACT
			bytesWritten, err := StreamWrite(stream, payload)
			require.NoError(t, err)

			// ASSERT
			require.Equal(t, len(expectedFrame), bytesWritten)
			require.NoError(t, <-readErr)
			require.Equal(t, payload, <-actualPayload)
		})

		t.Run("Noop", func(t *testing.T) {
			// ARRANGE
			writeCalls := 0
			stream := &streamStub{
				conn: &connStub{closed: false},
				writeFn: func(p []byte) (int, error) {
					writeCalls++
					return len(p), nil
				},
			}

			// ACT
			bytesWritten, err := StreamWrite(stream, nil)

			// ASSERT
			require.NoError(t, err)
			require.Equal(t, 0, bytesWritten)
			require.Equal(t, 0, writeCalls)
		})

		t.Run("ClosedStream", func(t *testing.T) {
			// ARRANGE
			stream := &streamStub{
				conn: &connStub{closed: true},
				writeFn: func(_ []byte) (int, error) {
					t.Fatal("write must not be called when conn is closed")
					return 0, nil
				},
			}

			// ACT
			bytesWritten, err := StreamWrite(stream, []byte("ignored"))

			// ASSERT
			require.Error(t, err)
			require.ErrorContains(t, err, "stream is closed")
			require.Equal(t, 0, bytesWritten)
		})

		t.Run("PartialWrite", func(t *testing.T) {
			// ARRANGE
			payload := []byte("abcde")
			expectedFrameLen := len(uint64ToUvarint(uint64(len(payload)))) + len(payload)

			stream := &streamStub{
				conn: &connStub{closed: false},
				writeFn: func(_ []byte) (int, error) {
					return 2, io.ErrClosedPipe
				},
			}

			// ACT
			bytesWritten, err := StreamWrite(stream, payload)

			// ASSERT
			require.Equal(t, 2, bytesWritten)
			require.Error(t, err)
			require.ErrorContains(t, err, "failed to write payload")
			require.ErrorContains(t, err, "(2/"+strconv.Itoa(expectedFrameLen)+" sent)")
		})
	})

	t.Run("StreamReadSized", func(t *testing.T) {
		t.Run("Read", func(t *testing.T) {
			// ARRANGE
			payload := []byte("read me")
			frame := append(uint64ToUvarint(uint64(len(payload))), payload...)
			reader := io.MultiReader(
				bytes.NewReader(frame[:1]),
				bytes.NewReader(frame[1:2]),
				bytes.NewReader(frame[2:]),
			)

			stream := &streamStub{
				conn:   &connStub{closed: false},
				readFn: reader.Read,
			}

			// ACT
			out, err := StreamReadSized(stream, 1024)

			// ASSERT
			require.NoError(t, err)
			require.Equal(t, payload, out)
		})

		t.Run("ClosedStream", func(t *testing.T) {
			// ARRANGE
			stream := &streamStub{
				conn: &connStub{closed: true},
			}

			// ACT
			out, err := StreamReadSized(stream, 1024)

			// ASSERT
			require.Error(t, err)
			require.ErrorContains(t, err, "stream is closed")
			require.Nil(t, out)
		})

		t.Run("TooLargePayload", func(t *testing.T) {
			// ARRANGE
			const maxSize = uint64(3)
			headerOnly := uint64ToUvarint(maxSize + 1)

			stream := &streamStub{
				conn:   &connStub{closed: false},
				readFn: bytes.NewReader(headerOnly).Read,
			}

			// ACT
			out, err := StreamReadSized(stream, maxSize)

			// ASSERT
			require.Error(t, err)
			require.ErrorContains(t, err, "payload is too large")
			require.Nil(t, out)
		})

		t.Run("LargerThanMaxStreamSize", func(t *testing.T) {
			// maxSize > MaxStreamSize must NOT be clamped — blocksync passes
			// RecvMessageCapacity (~100MB) and must be able to receive large blocks.
			payload := bytes.Repeat([]byte("x"), MaxStreamSize+1)
			frame := append(uint64ToUvarint(uint64(len(payload))), payload...)

			stream := &streamStub{
				conn:   &connStub{closed: false},
				readFn: bytes.NewReader(frame).Read,
			}

			out, err := StreamReadSized(stream, uint64(len(payload)))
			require.NoError(t, err)
			require.Equal(t, payload, out)
		})

		t.Run("InvalidHeader", func(t *testing.T) {
			// ARRANGE
			invalidHeader := []byte{0x80}
			stream := &streamStub{
				conn:   &connStub{closed: false},
				readFn: bytes.NewReader(invalidHeader).Read,
			}

			// ACT
			out, err := StreamReadSized(stream, 1024)

			// ASSERT
			require.Error(t, err)
			require.ErrorContains(t, err, "failed to read payload size")
			require.Nil(t, out)
		})

		t.Run("TruncatedPayload", func(t *testing.T) {
			// ARRANGE
			header := uint64ToUvarint(4)
			truncatedPayload := []byte("ab")
			reader := io.MultiReader(bytes.NewReader(header), bytes.NewReader(truncatedPayload))

			stream := &streamStub{
				conn:   &connStub{closed: false},
				readFn: reader.Read,
			}

			// ACT
			out, err := StreamReadSized(stream, 1024)

			// ASSERT
			require.Error(t, err)
			require.ErrorContains(t, err, "eof partial read")
			require.Nil(t, out)
		})
	})
}

func TestProtocolID(t *testing.T) {
	for _, tt := range []struct {
		channel  byte
		expected string
	}{
		{channel: 0x00, expected: "/p2p/cometbft/1.0.0/channel/0x00"},
		{channel: 0x01, expected: "/p2p/cometbft/1.0.0/channel/0x01"},
		{channel: 0x10, expected: "/p2p/cometbft/1.0.0/channel/0x10"},
		{channel: 0xff, expected: "/p2p/cometbft/1.0.0/channel/0xff"},
	} {
		require.Equal(t, protocol.ID(tt.expected), ProtocolID(tt.channel))
	}
}

func TestStreamReadSizedClose(t *testing.T) {
	t.Run("ReadTooLargePayload", func(t *testing.T) {
		// ARRANGE
		const maxSize = 100

		var (
			ctx     = context.Background()
			protoID = ProtocolID(0xAA)
			ports   = utils.GetFreePorts(t, 2)
			host1   = makeTestHost(t, ports[0], withLogging())
			host2   = makeTestHost(t, ports[1], withLogging())
		)

		// connect hosts
		require.NoError(t, host2.Connect(ctx, host1.AddrInfo()))

		readErr := make(chan error, 1)
		host1.SetStreamHandler(protoID, func(stream network.Stream) {
			defer stream.Close()

			_, err := StreamReadSizedClose(stream, maxSize)
			readErr <- err
		})

		// create stream
		stream, err := host2.NewStream(ctx, host1.ID(), protoID)
		require.NoError(t, err)
		t.Cleanup(func() { _ = stream.Close() })

		tooLargeHeader := uint64ToUvarint(maxSize + 1)

		// ACT
		_, err = stream.Write(tooLargeHeader)
		require.NoError(t, err)
		_ = stream.Close()

		// ASSERT
		err = <-readErr

		require.Error(t, err)
		require.ErrorContains(t, err, "payload is too large")
	})
}

type streamTestSuite struct {
	ctx     context.Context
	protoID protocol.ID
	hostA   *Host
	hostB   *Host
}

type connStub struct {
	network.Conn
	closed bool
}

func newStreamTestSuite(t *testing.T) *streamTestSuite {
	t.Helper()

	ports := utils.GetFreePorts(t, 2)

	hostA := makeTestHost(t, ports[0], withLogging())
	hostB := makeTestHost(t, ports[1], withLogging())
	protoID := ProtocolID(0xAA)

	// Register a default handler on both sides so protocol negotiation succeeds
	// even in tests that do not care about receiving data.
	noop := func(stream network.Stream) { _ = stream.Close() }
	hostA.SetStreamHandler(protoID, noop)
	hostB.SetStreamHandler(protoID, noop)

	ctx := context.Background()
	require.NoError(t, hostB.Connect(ctx, hostA.AddrInfo()))

	return &streamTestSuite{
		ctx:     ctx,
		protoID: protoID,
		hostA:   hostA,
		hostB:   hostB,
	}
}

// newStream creates a new stream from hostB to hostA.
func (ts *streamTestSuite) newStream(t *testing.T) network.Stream {
	t.Helper()

	stream, err := ts.hostB.NewStream(ts.ctx, ts.hostA.ID(), ts.protoID)
	require.NoError(t, err)

	return stream
}

type streamStub struct {
	network.Stream

	conn    network.Conn
	readFn  func([]byte) (int, error)
	writeFn func([]byte) (int, error)
}

func (s *streamStub) Conn() network.Conn {
	if s.conn != nil {
		return s.conn
	}

	return s.Stream.Conn()
}

func (s *streamStub) Read(p []byte) (int, error) {
	if s.readFn != nil {
		return s.readFn(p)
	}

	return s.Stream.Read(p)
}

func (s *streamStub) Write(p []byte) (int, error) {
	if s.writeFn != nil {
		return s.writeFn(p)
	}

	return s.Stream.Write(p)
}

func (s *streamStub) SetReadDeadline(t time.Time) error {
	if s.Stream == nil {
		return nil
	}

	return s.Stream.SetReadDeadline(t)
}

func (s *streamStub) SetWriteDeadline(t time.Time) error {
	if s.Stream == nil {
		return nil
	}

	return s.Stream.SetWriteDeadline(t)
}

func (c *connStub) IsClosed() bool { return c.closed }

type streamProgressSuite struct {
	suite.Suite
}

func TestStreamProgressSuite(t *testing.T) {
	suite.Run(t, new(streamProgressSuite))
}

func (s *streamProgressSuite) TestReadTimeout() {
	const timeout = 500 * time.Millisecond

	payload := patternBytes(24)
	frame := append(uint64ToUvarint(uint64(len(payload))), payload...)

	testCases := []struct {
		name      string
		write     func(stream network.Stream)
		expectErr bool
	}{
		{
			name:      "stalls before the length prefix",
			write:     func(stream network.Stream) { _, _ = stream.Write([]byte{0x80}) },
			expectErr: true,
		},
		{
			name:      "stalls after the length prefix",
			write:     func(stream network.Stream) { _, _ = stream.Write(frame[:4]) },
			expectErr: true,
		},
		{
			name: "slow writer that keeps making progress",
			write: func(stream network.Stream) {
				for _, b := range frame[:len(frame)-1] {
					_, _ = stream.Write([]byte{b})
					time.Sleep(timeout / 10)
				}
				_, _ = stream.Write(frame[len(frame)-1:])
				_ = stream.CloseWrite()
			},
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			ts := newStreamTestSuite(s.T())
			protoID := ProtocolID(0xAB)

			type result struct {
				payload []byte
				err     error
			}

			results := make(chan result, 1)
			ts.hostA.SetStreamHandler(protoID, func(stream network.Stream) {
				out, err := streamReadSizedClose(stream, 1024, timeout)
				results <- result{payload: out, err: err}
			})

			stream, err := ts.hostB.NewStream(ts.ctx, ts.hostA.ID(), protoID)
			s.Require().NoError(err)
			s.T().Cleanup(func() { _ = stream.Reset() })

			tc.write(stream)

			select {
			case res := <-results:
				if tc.expectErr {
					s.ErrorContains(res.err, "deadline")
					return
				}

				s.Require().NoError(res.err)
				s.Equal(payload, res.payload)
			case <-time.After(10 * time.Second):
				s.Fail("read did not finish")
			}
		})
	}
}

func (s *streamProgressSuite) TestWriteTimeout() {
	const timeout = 500 * time.Millisecond

	payload := patternBytes(2 << 20)

	testCases := []struct {
		name      string
		read      func(stream network.Stream) []byte
		expectErr bool
	}{
		{
			name: "reader stalls",
			read: func(network.Stream) []byte {
				time.Sleep(4 * timeout)
				return nil
			},
			expectErr: true,
		},
		{
			name: "slow reader that keeps making progress",
			read: func(stream network.Stream) []byte {
				var out []byte
				buf := make([]byte, 64<<10)
				for {
					n, err := stream.Read(buf)
					out = append(out, buf[:n]...)
					if err != nil {
						return out
					}
					time.Sleep(timeout / 10)
				}
			},
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			ts := newStreamTestSuite(s.T())
			protoID := ProtocolID(0xAC)

			received := make(chan []byte, 1)
			ts.hostA.SetStreamHandler(protoID, func(stream network.Stream) {
				received <- tc.read(stream)
				_ = stream.Reset()
			})

			stream, err := ts.hostB.NewStream(ts.ctx, ts.hostA.ID(), protoID)
			s.Require().NoError(err)

			start := time.Now()
			err = streamWriteClose(stream, payload, timeout)

			if tc.expectErr {
				s.ErrorContains(err, "deadline")
				s.Less(time.Since(start), 3*timeout)
				return
			}

			s.Require().NoError(err)
			s.Greater(time.Since(start), 2*timeout)
			s.Equal(append(uint64ToUvarint(uint64(len(payload))), payload...), <-received)
		})
	}
}

func (s *streamProgressSuite) TestDeadlinesClearedOnReturn() {
	const timeout = 300 * time.Millisecond

	var (
		ts      = newStreamTestSuite(s.T())
		protoID = ProtocolID(0xAD)
		tail    = []byte("tail")
	)

	readErr := make(chan error, 1)
	ts.hostA.SetStreamHandler(protoID, func(stream network.Stream) {
		defer func() { _ = stream.Reset() }()

		if _, err := streamReadSized(stream, 1024, timeout); err != nil {
			readErr <- err
			return
		}

		_, err := io.ReadFull(stream, make([]byte, len(tail)))
		readErr <- err
	})

	stream, err := ts.hostB.NewStream(ts.ctx, ts.hostA.ID(), protoID)
	s.Require().NoError(err)
	s.T().Cleanup(func() { _ = stream.Reset() })

	_, err = streamWrite(stream, []byte("head"), timeout)
	s.Require().NoError(err)

	time.Sleep(3 * timeout)

	_, err = stream.Write(tail)
	s.Require().NoError(err)
	s.NoError(<-readErr)
}

func (s *streamProgressSuite) TestReadExactly() {
	testCases := []struct {
		name        string
		size        uint64
		available   int
		readErr     error
		expectedErr string
	}{
		{name: "empty", size: 0},
		{name: "single segment", size: 100, available: 100},
		{name: "truncated single segment", size: 100, available: 50, expectedErr: "eof partial read (50/100 bytes)"},
		{name: "exactly one segment", size: readSegmentSize, available: readSegmentSize},
		{name: "one byte past one segment", size: readSegmentSize + 1, available: readSegmentSize + 1},
		{name: "several segments", size: 3*readSegmentSize + 7, available: 3*readSegmentSize + 7},
		{name: "announced size larger than the data", size: 100 << 20, available: readSegmentSize + 100, expectedErr: "eof partial read (1048676/104857600 bytes)"},
		{name: "reader fails mid-message", size: 200, available: 100, readErr: errors.New("boom"), expectedErr: "failed to read payload (read 100/200 bytes): boom"},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			data := patternBytes(tc.available)
			source := io.Reader(bytes.NewReader(data))
			if tc.readErr != nil {
				source = io.MultiReader(source, iotest.ErrReader(tc.readErr))
			}
			r := &readSizeRecorder{r: iotest.HalfReader(source)}

			out, err := readExactly(r, tc.size)

			s.LessOrEqual(r.maxBuf, readSegmentSize)
			if tc.expectedErr != "" {
				s.ErrorContains(err, tc.expectedErr)
				return
			}

			s.Require().NoError(err)
			s.Equal(data, out)
		})
	}
}

type readSizeRecorder struct {
	r      io.Reader
	maxBuf int
}

func (r *readSizeRecorder) Read(p []byte) (int, error) {
	r.maxBuf = max(r.maxBuf, len(p))

	return r.r.Read(p)
}

func patternBytes(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*31 + i>>8)
	}

	return out
}
