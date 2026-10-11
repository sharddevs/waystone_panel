package helper

import (
	"encoding/binary"
	"errors"
	"io"

	"google.golang.org/protobuf/proto"
)

// MaxFrameSize is the largest message either side will send or accept.
const MaxFrameSize = 64 << 10

// ErrFrameTooLarge is returned for a frame longer than MaxFrameSize.
var ErrFrameTooLarge = errors.New("helper: frame too large")

// WriteFrame writes m as a 4-byte big-endian length followed by its
// protobuf encoding, in a single Write. A message larger than MaxFrameSize
// is refused with ErrFrameTooLarge and nothing is written.
func WriteFrame(w io.Writer, m proto.Message) error {
	data, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	if len(data) > MaxFrameSize {
		return ErrFrameTooLarge
	}
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)
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
// if there was no input at all and io.ErrUnexpectedEOF if the body is cut
// short.
func ReadFrame(r io.Reader, m proto.Message) error {
	var header [4]byte

	_, err := io.ReadFull(r, header[:])
	if err != nil {
		return err
	}
	length := binary.BigEndian.Uint32(header[:])

	// Check the length before allocating the body. Otherwise any local user
	// could make root allocate 4 GiB by sending four bytes.
	if length > MaxFrameSize {
		return ErrFrameTooLarge
	}
	data := make([]byte, int(length))

	// io.ReadFull returns plain io.EOF when it read nothing, but here the
	// header promised a body, so that's still an unexpected end.
	_, err = io.ReadFull(r, data)
	if errors.Is(err, io.EOF) && length > 0 {
		return io.ErrUnexpectedEOF
	}
	if err != nil {
		return err
	}
	return proto.Unmarshal(data, m)
}
