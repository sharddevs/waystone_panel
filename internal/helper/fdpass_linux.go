// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"

	"google.golang.org/protobuf/proto"
)

// WriteFrameFD writes m like WriteFrame. If f is not nil, its descriptor
// goes along in the same sendmsg (SCM_RIGHTS), so it arrives together with
// the frame's first bytes. Sending f doesn't close it; it still belongs to
// the caller.
func WriteFrameFD(conn *net.UnixConn, m proto.Message, f *os.File) error {
	frame, err := marshalFrame(m)
	if err != nil {
		return err
	}

	var oob []byte
	if f != nil {
		oob = syscall.UnixRights(int(f.Fd()))
	}

	n, oobn, err := conn.WriteMsgUnix(frame, oob, nil)
	if err != nil {
		return err
	}
	if n != len(frame) || oobn != len(oob) {
		return io.ErrShortWrite
	}

	return nil
}

// ReadFrameFD reads one frame into m like ReadFrame and returns the
// descriptor that came with it, or nil if none did. More than one
// descriptor, or any other kind of control message, is an error, and on
// an error the descriptors it already parsed are closed again.
func ReadFrameFD(conn *net.UnixConn, m proto.Message) (*os.File, error) {
	var header [4]byte
	// On Linux amd64 CmsgSpace(4) is 24 bytes, which is room for two
	// descriptors because of alignment, so the len(fds) > 1 check below is
	// what actually refuses a second one. If even more are sent, the kernel
	// drops the ones that don't fit and sets MSG_CTRUNC.
	oob := make([]byte, syscall.CmsgSpace(4))

	// Go sets MSG_CMSG_CLOEXEC here on Linux, so a descriptor we receive
	// is never inherited by a program the helper starts.
	n, oobn, flags, _, err := conn.ReadMsgUnix(header[:], oob)
	if err != nil {
		return nil, err
	}

	var fds []int
	defer func() {
		for _, fd := range fds {
			syscall.Close(fd)
		}
	}()

	// Collect the descriptors before looking at the frame, so the defer
	// above closes them even when the frame turns out to be bad.
	if oobn > 0 {
		messages, parseErr := syscall.ParseSocketControlMessage(oob[:oobn])
		if parseErr != nil {
			return nil, fmt.Errorf("helper: parse control messages: %w", parseErr)
		}

		var invalidControl bool

		for _, message := range messages {
			if message.Header.Level != syscall.SOL_SOCKET ||
				message.Header.Type != syscall.SCM_RIGHTS {
				invalidControl = true
				continue
			}

			rights, parseErr := syscall.ParseUnixRights(&message)
			if parseErr != nil {
				return nil, fmt.Errorf("helper: parse descriptor rights: %w", parseErr)
			}

			fds = append(fds, rights...)
		}

		if invalidControl {
			return nil, fmt.Errorf("helper: unexpected control message")
		}
	}

	if flags&syscall.MSG_CTRUNC != 0 {
		return nil, fmt.Errorf("helper: truncated control message")
	}

	if len(fds) > 1 {
		return nil, fmt.Errorf("helper: multiple input descriptors")
	}
	if n == 0 {
		return nil, io.EOF
	}
	// ReadMsgUnix can return fewer than 4 bytes. The rest comes in with
	// plain reads; the kernel throws away any descriptor attached to bytes
	// read without a control buffer, so none can sneak in that way.
	if _, err := io.ReadFull(conn, header[n:]); err != nil {
		return nil, err
	}

	length := binary.BigEndian.Uint32(header[:])
	if length > MaxFrameSize {
		return nil, ErrFrameTooLarge
	}

	data := make([]byte, int(length))
	if _, err := io.ReadFull(conn, data); err != nil {
		if errors.Is(err, io.EOF) && length > 0 {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}

	if err := proto.Unmarshal(data, m); err != nil {
		return nil, err
	}

	if len(fds) == 0 {
		return nil, nil
	}

	// The caller owns the descriptor now. Setting fds to nil keeps the
	// deferred cleanup from closing it.
	fd := fds[0]
	fds = nil

	return os.NewFile(uintptr(fd), "install-input"), nil
}
