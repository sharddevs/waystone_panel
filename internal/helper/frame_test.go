package helper

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"google.golang.org/protobuf/proto"
)

func TestFrameRoundTrip(t *testing.T) {
	in := &helperv1.Request{
		OperationId: "0192a5c4-7d3e-7b1a-9c2f-3e4d5f6a7b8c",
		ServerId:    "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		Action:      helperv1.Action_ACTION_START,
	}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	var out helperv1.Request
	if err := ReadFrame(&buf, &out); err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !proto.Equal(in, &out) {
		t.Errorf("round trip changed the message: got %v, want %v", &out, in)
	}
}

func TestFrameLayout(t *testing.T) {
	in := &helperv1.Request{ServerId: "abc"}
	body, err := proto.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got := buf.Bytes()
	if len(got) != 4+len(body) {
		t.Fatalf("frame is %d bytes, want 4-byte length + %d-byte body", len(got), len(body))
	}
	if n := binary.BigEndian.Uint32(got[:4]); int(n) != len(body) {
		t.Errorf("length prefix = %d, want %d (big-endian)", n, len(body))
	}
	if !bytes.Equal(got[4:], body) {
		t.Errorf("body is not the protobuf encoding of the message")
	}
}

// failingReader fails the test if anything reads past the header.
type failingReader struct{ t *testing.T }

func (r failingReader) Read([]byte) (int, error) {
	r.t.Error("ReadFrame read the body of an oversized frame; it must check the length first")
	return 0, io.EOF
}

func TestReadFrameRejectsOversizeBeforeReadingBody(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], MaxFrameSize+1)
	r := io.MultiReader(bytes.NewReader(header[:]), failingReader{t})
	var out helperv1.Request
	if err := ReadFrame(r, &out); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("ReadFrame = %v, want ErrFrameTooLarge", err)
	}
}

func TestReadFrameAcceptsMaxSize(t *testing.T) {
	// 1 tag byte + 3 length bytes + the string = exactly MaxFrameSize.
	msg := &helperv1.Request{ServerId: strings.Repeat("x", MaxFrameSize-4)}
	if proto.Size(msg) != MaxFrameSize {
		t.Fatalf("test setup: message is %d bytes, want %d", proto.Size(msg), MaxFrameSize)
	}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, msg); err != nil {
		t.Fatalf("WriteFrame at exactly MaxFrameSize: %v", err)
	}
	var out helperv1.Request
	if err := ReadFrame(&buf, &out); err != nil {
		t.Fatalf("ReadFrame at exactly MaxFrameSize: %v", err)
	}
}

func TestReadFrameTruncatedBody(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 10)
	r := io.MultiReader(bytes.NewReader(header[:]), bytes.NewReader([]byte{1, 2, 3}))
	var out helperv1.Request
	if err := ReadFrame(r, &out); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("ReadFrame = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestReadFrameNoInput(t *testing.T) {
	var out helperv1.Request
	if err := ReadFrame(bytes.NewReader(nil), &out); !errors.Is(err, io.EOF) {
		t.Errorf("ReadFrame on empty input = %v, want io.EOF", err)
	}
}

func TestReadFrameGarbageBody(t *testing.T) {
	body := []byte{0xff, 0xff, 0xff, 0xff, 0xff}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(body)))
	buf.Write(body)
	var out helperv1.Request
	if err := ReadFrame(&buf, &out); err == nil {
		t.Error("ReadFrame accepted bytes that are not a valid protobuf message")
	}
}

func TestWriteFrameRejectsOversize(t *testing.T) {
	msg := &helperv1.Request{ServerId: strings.Repeat("x", MaxFrameSize)}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, msg); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("WriteFrame = %v, want ErrFrameTooLarge", err)
	}
	if buf.Len() != 0 {
		t.Errorf("WriteFrame wrote %d bytes of an oversized frame, want 0", buf.Len())
	}
}
func TestReadFrameHeaderOnly(t *testing.T) {
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, 10)

	var resp helperv1.Response
	err := ReadFrame(bytes.NewReader(header), &resp)

	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadFrame() error = %v, want io.ErrUnexpectedEOF", err)
	}
}
