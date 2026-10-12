// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package helper

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"google.golang.org/protobuf/proto"
)

// connPair returns two connected Unix stream sockets. A socketpair needs no
// file on disk and no listener, which keeps these tests simple.
func connPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn := func(fd int) *net.UnixConn {
		f := os.NewFile(uintptr(fd), "socketpair")
		defer f.Close() // net.FileConn dups the descriptor, so ours can go
		c, err := net.FileConn(f)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c.(*net.UnixConn)
	}
	return conn(fds[0]), conn(fds[1])
}

// openFDs counts this process's open descriptors by listing /proc/self/fd.
// Comparing the count before and after a call is how we catch leaks.
func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// contentFile returns a temp file holding content. A passed descriptor
// shares its file offset with ours, so we seek back to the start or the
// receiver would read nothing.
func contentFile(t *testing.T, content string) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "content")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	return f
}

func installRequest() *helperv1.Request {
	req := validRequest(helperv1.Action_ACTION_INSTALL_FILE)
	req.File = installSpec()
	return req
}

func TestFrameFDRoundTrip(t *testing.T) {
	a, b := connPair(t)
	if err := WriteFrameFD(a, installRequest(), contentFile(t, "jar bytes")); err != nil {
		t.Fatalf("WriteFrameFD: %v", err)
	}
	got := new(helperv1.Request)
	f, err := ReadFrameFD(b, got)
	if err != nil || f == nil {
		t.Fatalf("ReadFrameFD = %v, %v; want a file", f, err)
	}
	defer f.Close()
	if !proto.Equal(got, installRequest()) {
		t.Errorf("request = %v, want %v", got, installRequest())
	}
	content, err := io.ReadAll(f)
	if err != nil || string(content) != "jar bytes" {
		t.Errorf("received file reads %q, %v", content, err)
	}

	// Go's ReadMsgUnix asks for MSG_CMSG_CLOEXEC, so the received descriptor
	// can't leak into a program the helper starts by accident.
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_GETFD, 0)
	if errno != 0 || flags&syscall.FD_CLOEXEC == 0 {
		t.Errorf("received descriptor flags = %#x, %v; want FD_CLOEXEC", flags, errno)
	}
}

func TestFrameFDWithoutDescriptor(t *testing.T) {
	a, b := connPair(t)
	if err := WriteFrameFD(a, validRequest(helperv1.Action_ACTION_STATUS), nil); err != nil {
		t.Fatalf("WriteFrameFD: %v", err)
	}
	got := new(helperv1.Request)
	f, err := ReadFrameFD(b, got)
	if err != nil || f != nil {
		t.Fatalf("ReadFrameFD = %v, %v; want no file", f, err)
	}
	if got.GetAction() != helperv1.Action_ACTION_STATUS {
		t.Errorf("request = %v", got)
	}

	// ReadFrameFD also reads frames written by plain WriteFrame.
	if err := WriteFrame(a, validRequest(helperv1.Action_ACTION_STOP)); err != nil {
		t.Fatal(err)
	}
	if f, err := ReadFrameFD(b, got); err != nil || f != nil || got.GetAction() != helperv1.Action_ACTION_STOP {
		t.Errorf("after WriteFrame: %v, %v, %v", f, err, got)
	}
}

func TestReadFrameFDNoInput(t *testing.T) {
	a, b := connPair(t)
	a.Close()
	if f, err := ReadFrameFD(b, new(helperv1.Request)); !errors.Is(err, io.EOF) || f != nil {
		t.Errorf("ReadFrameFD on a closed peer = %v, %v; want io.EOF", f, err)
	}
}

// The descriptor comes with the first two header bytes and the rest arrives
// in a separate write, so ReadFrameFD has to finish the header with plain
// reads and still keep the descriptor.
func TestReadFrameFDSplitHeader(t *testing.T) {
	a, b := connPair(t)
	data, err := proto.Marshal(installRequest())
	if err != nil {
		t.Fatal(err)
	}
	frame := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	frame = append(frame, data...)
	if _, _, err := a.WriteMsgUnix(frame[:2], syscall.UnixRights(int(contentFile(t, "x").Fd())), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write(frame[2:]); err != nil {
		t.Fatal(err)
	}
	got := new(helperv1.Request)
	f, err := ReadFrameFD(b, got)
	if err != nil || f == nil || !proto.Equal(got, installRequest()) {
		t.Fatalf("ReadFrameFD = %v, %v, %v; want the request and a file", f, err, got)
	}
	f.Close()
}

func TestReadFrameFDRefusesTwoDescriptors(t *testing.T) {
	a, b := connPair(t)
	one, two := contentFile(t, "1"), contentFile(t, "2")
	data, err := proto.Marshal(installRequest())
	if err != nil {
		t.Fatal(err)
	}
	frame := append(binary.BigEndian.AppendUint32(nil, uint32(len(data))), data...)
	// Two descriptors still fit in ReadFrameFD's control buffer (CmsgSpace(4)
	// is 24 bytes on amd64 because of alignment), so this hits the "more than
	// one" check, not truncation.
	before := openFDs(t)
	if _, _, err := a.WriteMsgUnix(frame, syscall.UnixRights(int(one.Fd()), int(two.Fd())), nil); err != nil {
		t.Fatal(err)
	}
	f, err := ReadFrameFD(b, new(helperv1.Request))
	if err == nil || f != nil {
		t.Errorf("two descriptors: %v, %v; want an error and no file", f, err)
	}
	if after := openFDs(t); after != before {
		t.Errorf("%d descriptors open after the refusal, %d before: received ones leaked", after, before)
	}
}

func TestReadFrameFDRefusesOversizeAndClosesDescriptor(t *testing.T) {
	a, b := connPair(t)
	f := contentFile(t, "x")
	before := openFDs(t)
	// Only the header is sent. The length check has to refuse it before
	// any body is read.
	header := binary.BigEndian.AppendUint32(nil, MaxFrameSize+1)
	if _, _, err := a.WriteMsgUnix(header, syscall.UnixRights(int(f.Fd())), nil); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrameFD(b, new(helperv1.Request))
	if !errors.Is(err, ErrFrameTooLarge) || got != nil {
		t.Errorf("oversized frame: %v, %v; want ErrFrameTooLarge and no file", got, err)
	}
	if after := openFDs(t); after != before {
		t.Errorf("%d descriptors open after the refusal, %d before", after, before)
	}
}

func TestWriteFrameFDRejectsOversize(t *testing.T) {
	a, _ := connPair(t)
	req := installRequest()
	// MaxFrameSize bytes of content plus the other fields is over the limit.
	req.File.Content = make([]byte, MaxFrameSize)
	if err := WriteFrameFD(a, req, contentFile(t, "x")); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("WriteFrameFD = %v, want ErrFrameTooLarge", err)
	}
}

// Sending a descriptor doesn't close it. The caller still owns f.
func TestWriteFrameFDKeepsTheFileOpen(t *testing.T) {
	a, b := connPair(t)
	f := contentFile(t, "x")
	if err := WriteFrameFD(a, installRequest(), f); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Stat(); err != nil {
		t.Errorf("file after sending: %v", err)
	}
	// Read the frame so the received copy gets closed as well.
	if got, err := ReadFrameFD(b, new(helperv1.Request)); err == nil {
		got.Close()
	}
}
func TestReadFrameFDTruncationClosesDescriptors(t *testing.T) {
	a, b := connPair(t)

	one := contentFile(t, "one")
	two := contentFile(t, "two")
	three := contentFile(t, "three")

	data, err := proto.Marshal(installRequest())
	if err != nil {
		t.Fatal(err)
	}

	frame := append(
		binary.BigEndian.AppendUint32(nil, uint32(len(data))),
		data...,
	)

	// Three descriptors don't fit in the control buffer, so the kernel sets
	// MSG_CTRUNC and drops the extras. The ones that did arrive still have
	// to be closed.
	before := openFDs(t)

	rights := syscall.UnixRights(
		int(one.Fd()),
		int(two.Fd()),
		int(three.Fd()),
	)

	if _, _, err := a.WriteMsgUnix(frame, rights, nil); err != nil {
		t.Fatal(err)
	}

	f, err := ReadFrameFD(b, new(helperv1.Request))
	if err == nil {
		if f != nil {
			f.Close()
		}
		t.Fatal("expected multiple descriptors to be rejected")
	}
	if f != nil {
		f.Close()
		t.Fatal("received a file on rejected request")
	}

	if after := openFDs(t); after != before {
		t.Errorf("descriptor leak: before=%d after=%d", before, after)
	}
}
