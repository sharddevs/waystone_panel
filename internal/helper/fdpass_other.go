// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux

package helper

import (
	"errors"
	"net"
	"os"

	"google.golang.org/protobuf/proto"
)

// ErrDescriptorUnsupported is returned by WriteFrameFD when it is asked to
// send a descriptor. Passing descriptors is only done on Linux.
var ErrDescriptorUnsupported = errors.New("helper: passing a file descriptor is only supported on Linux")

// WriteFrameFD is the non-Linux version. The build tag at the top keeps
// this file out of Linux builds, where fdpass_linux.go is used instead.
// With f == nil it is just WriteFrame; otherwise it sends nothing and
// returns ErrDescriptorUnsupported.
func WriteFrameFD(conn *net.UnixConn, m proto.Message, f *os.File) error {
	if f != nil {
		return ErrDescriptorUnsupported
	}

	return WriteFrame(conn, m)
}

// ReadFrameFD is just ReadFrame here and never returns a descriptor.
func ReadFrameFD(conn *net.UnixConn, m proto.Message) (*os.File, error) {
	if err := ReadFrame(conn, m); err != nil {
		return nil, err
	}

	return nil, nil
}
