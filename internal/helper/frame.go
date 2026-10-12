// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"encoding/binary"
	"errors"
	"io"

	"google.golang.org/protobuf/proto"
)

// MaxFrameSize is the largest encoded message either side sends or accepts.
const MaxFrameSize = 64 << 10

// ErrFrameTooLarge is returned for a message bigger than MaxFrameSize, on
// both the write and the read side.
var ErrFrameTooLarge = errors.New("helper: frame too large")

// marshalFrame encodes m and puts its length in front as 4 big-endian bytes.
// WriteFrame and WriteFrameFD both use it, so the format lives in one place.
func marshalFrame(m proto.Message) ([]byte, error) {
	data, err := proto.Marshal(m)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFrameSize {
		return nil, ErrFrameTooLarge
	}

	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)

	return frame, nil
}

// WriteFrame writes m as one frame (length prefix, then the protobuf bytes)
// in a single Write. If m is too big it returns ErrFrameTooLarge and writes
// nothing.
func WriteFrame(w io.Writer, m proto.Message) error {
	frame, err := marshalFrame(m)
	if err != nil {
		return err
	}

	n, err := w.Write(frame)
	if err != nil {
		return err
	}
	if n != len(frame) {
		return io.ErrShortWrite
	}
	return nil
}

// ReadFrame reads one frame written by WriteFrame into m. It returns io.EOF
// if the stream ended before any byte arrived, and io.ErrUnexpectedEOF if it
// ended partway through a frame.
func ReadFrame(r io.Reader, m proto.Message) error {
	var header [4]byte

	_, err := io.ReadFull(r, header[:])
	if err != nil {
		return err
	}
	length := binary.BigEndian.Uint32(header[:])

	// Check the length before allocating. Otherwise a 4-byte header alone
	// could make the helper (running as root) allocate 4 GiB.
	if length > MaxFrameSize {
		return ErrFrameTooLarge
	}
	data := make([]byte, int(length))

	// io.ReadFull gives plain io.EOF when it reads nothing at all, but the
	// header already promised a body, so for us that's an unexpected end.
	_, err = io.ReadFull(r, data)
	if errors.Is(err, io.EOF) && length > 0 {
		return io.ErrUnexpectedEOF
	}
	if err != nil {
		return err
	}
	return proto.Unmarshal(data, m)
}
