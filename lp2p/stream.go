package lp2p

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/pkg/errors"
)

// ProtocolIDPrefix is the prefix for all protocol IDs.
const ProtocolIDPrefix = "/p2p/cometbft/1.0.0"

// TimeoutStream is the timeout for opening a stream and the default time
// a single stream read call or 16 KB write chunk may take.
const TimeoutStream = 10 * time.Second

// MaxStreamSize is the global maximum size of a stream.
// Protocols should configure their own maximum size.
const MaxStreamSize = 4 * (1 << 20)

const (
	writeChunkSize  = 16 << 10
	readSegmentSize = 1 << 20
)

type progressReader struct {
	network.Stream
	timeout time.Duration
}

func (r progressReader) Read(p []byte) (int, error) {
	if err := r.SetReadDeadline(time.Now().Add(r.timeout)); err != nil {
		return 0, err
	}

	return r.Stream.Read(p)
}

func writeWithProgress(s network.Stream, p []byte, timeout time.Duration) (int, error) {
	written := 0

	for written < len(p) {
		if err := s.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
			return written, err
		}

		n, err := s.Write(p[written:min(len(p), written+writeChunkSize)])
		written += n
		if err != nil {
			return written, err
		}
	}

	return written, nil
}

// ProtocolID returns the protocol ID for a given channel
// Byte is used for compatibility with the original CometBFT implementation.
func ProtocolID(channelID byte) protocol.ID {
	return protocol.ID(
		fmt.Sprintf("%s/channel/0x%02x", ProtocolIDPrefix, channelID),
	)
}

// StreamWrite sends payload over a stream w/o waiting for a response.
// Only guarantees that the recipient will receive the bytes (no "message processed" guarantee).
// It doesn't control stream's lifecycle, so it's up to the caller to close the stream.
// The write fails if a 16 KB chunk is not accepted within TimeoutStream. It overrides any write deadline set on
// the stream and clears it on return.
func StreamWrite(s network.Stream, data []byte) (int, error) {
	return streamWrite(s, data, TimeoutStream)
}

func streamWrite(s network.Stream, data []byte, timeout time.Duration) (int, error) {
	defer func() { _ = s.SetWriteDeadline(time.Time{}) }()

	switch {
	case len(data) == 0:
		// noop
		return 0, nil
	case s.Conn().IsClosed():
		return 0, fmt.Errorf("stream is closed")
	}

	// [header(content_len) | payload]
	var (
		header  = uint64ToUvarint(uint64(len(data)))
		payload = append(header, data...)
	)

	bytesWritten, err := writeWithProgress(s, payload, timeout)
	if err != nil {
		err = errors.Wrapf(err, "failed to write payload (%d/%d sent)", bytesWritten, len(payload))
	}

	return bytesWritten, err
}

// StreamWriteClose sends payload over a stream and closes it right after.
// The caller doesn't expect a response in this case.
// Also, resets the stream on both ends in case of error.
// Timeouts and deadlines behave as in StreamWrite.
func StreamWriteClose(s network.Stream, data []byte) error {
	return streamWriteClose(s, data, TimeoutStream)
}

func streamWriteClose(s network.Stream, data []byte, timeout time.Duration) (err error) {
	defer func() {
		if err != nil {
			// nukes broken stream on both ends
			_ = s.Reset()
		}
	}()

	if _, err := streamWrite(s, data, timeout); err != nil {
		return errors.Wrap(err, "send failed")
	}

	if err := closeStream(s); err != nil {
		return errors.Wrap(err, "closeStream")
	}

	return nil
}

// StreamRead reads payload from a stream.
// It doesn't control stream's lifecycle, so it's up to the caller to close the stream.
// Note: this method doesn't enforce any size limits! Use StreamReadSized instead.
// Timeouts and deadlines behave as in StreamReadSized.
func StreamRead(s network.Stream) ([]byte, error) {
	return StreamReadSized(s, MaxStreamSize)
}

// StreamReadSized reads payload from a stream with a maximum size.
// It doesn't control stream's lifecycle, so it's up to the caller to close the stream.
// The read fails if a read call returns nothing within TimeoutStream. It overrides any read deadline set on the
// stream and clears it on return.
func StreamReadSized(s network.Stream, maxSize uint64) ([]byte, error) {
	return streamReadSized(s, maxSize, TimeoutStream)
}

func streamReadSized(s network.Stream, maxSize uint64, timeout time.Duration) ([]byte, error) {
	defer func() { _ = s.SetReadDeadline(time.Time{}) }()

	if s.Conn().IsClosed() {
		return nil, fmt.Errorf("stream is closed")
	}

	reader := bufio.NewReader(progressReader{Stream: s, timeout: timeout})

	// in bytes
	payloadSize, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read payload size")
	}

	// Honor the caller's limit; MaxStreamSize is a fallback, not a hard cap.
	payloadLimit := maxSize
	if payloadLimit == 0 {
		payloadLimit = MaxStreamSize
	}

	if payloadSize > payloadLimit {
		return nil, errors.Errorf("payload is too large (got %d, max %d)", payloadSize, payloadLimit)
	}

	payload, err := readExactly(reader, payloadSize)
	if err != nil {
		return nil, err
	}

	return payload, nil
}

// StreamReadClose reads payload from a stream and closes it right after.
// Also, resets the stream on both ends in case of error.
// Timeouts and deadlines behave as in StreamReadSized.
func StreamReadClose(s network.Stream) (payload []byte, err error) {
	return StreamReadSizedClose(s, MaxStreamSize)
}

// StreamReadSizedClose reads payload from a stream and closes it right after with a maximum size.
// Also, resets the stream on both ends in case of error.
// Timeouts and deadlines behave as in StreamReadSized.
func StreamReadSizedClose(s network.Stream, maxSize uint64) ([]byte, error) {
	return streamReadSizedClose(s, maxSize, TimeoutStream)
}

func streamReadSizedClose(s network.Stream, maxSize uint64, timeout time.Duration) (payload []byte, err error) {
	defer func() {
		if err != nil {
			// nukes broken stream on both ends
			_ = s.Reset()
		}
	}()

	payload, err = streamReadSized(s, maxSize, timeout)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read payload")
	}

	if err := closeStream(s); err != nil {
		return nil, errors.Wrap(err, "closeStream")
	}

	return payload, nil
}

// readExactly reads exactly $size bytes; beyond 1 MB, memory grows with the bytes received, not with the announced size.
func readExactly(r io.Reader, size uint64) ([]byte, error) {
	if size <= readSegmentSize {
		out := make([]byte, size)
		if n, err := io.ReadFull(r, out); err != nil {
			return nil, wrapReadErr(err, uint64(n), size)
		}

		return out, nil
	}

	segments := make([][]byte, 0, size/readSegmentSize+1)

	for received := uint64(0); received < size; {
		segment := make([]byte, min(size-received, readSegmentSize))
		n, err := io.ReadFull(r, segment)
		received += uint64(n)
		if err != nil {
			return nil, wrapReadErr(err, received, size)
		}

		segments = append(segments, segment)
	}

	out := make([]byte, 0, size)
	for _, segment := range segments {
		out = append(out, segment...)
	}

	return out, nil
}

func wrapReadErr(err error, received, size uint64) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errors.Wrapf(err, "eof partial read (%d/%d bytes)", received, size)
	}

	return errors.Wrapf(err, "failed to read payload (read %d/%d bytes)", received, size)
}

func closeStream(s network.Stream) error {
	err := s.Close()
	switch {
	case isErrCancelled(err):
		// expected if peer canceled the stream
	case err != nil:
		return errors.Wrap(err, "failed to close stream")
	}

	return nil
}

// go-libp2p doesn't have a sentinel error for this!
func isErrCancelled(err error) bool {
	if err == nil {
		return false
	}

	const pattern = "close called for canceled stream"

	return strings.Contains(err.Error(), pattern)
}

func uint64ToUvarint(len uint64) []byte {
	out := make([]byte, binary.MaxVarintLen64)
	bytesWritten := binary.PutUvarint(out, len)

	return out[:bytesWritten]
}
