// SPDX-License-Identifier: AGPL-3.0-only

//go:build !unix

package agenttls

import (
	"crypto/tls"
	"errors"
)

// KeyDir needs openat and fstat (keydir.go). v0.1 runs on Linux only;
// this keeps the other builds compiling.
type KeyDir struct{}

var errNoKeyDir = errors.New("agent link key directories need a Unix system")

func OpenKeyDir(path string, create bool, owners ...int) (*KeyDir, error) { return nil, errNoKeyDir }

func (d *KeyDir) Sub(name string, create bool, owners ...int) (*KeyDir, error) {
	return nil, errNoKeyDir
}
func (d *KeyDir) Path() string                           { return "" }
func (d *KeyDir) Close() error                           { return nil }
func (d *KeyDir) Load() (tls.Certificate, error)         { return tls.Certificate{}, errNoKeyDir }
func (d *KeyDir) LoadOrCreate() (tls.Certificate, error) { return tls.Certificate{}, errNoKeyDir }
func (d *KeyDir) Write(c tls.Certificate) error          { return errNoKeyDir }
func (d *KeyDir) MoveTo(dst *KeyDir, uid, gid int) error { return errNoKeyDir }
func (d *KeyDir) RemoveSub(name string) error            { return errNoKeyDir }
func (d *KeyDir) Chown(uid, gid int) error               { return errNoKeyDir }
