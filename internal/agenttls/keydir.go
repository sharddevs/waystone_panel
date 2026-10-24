// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package agenttls

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"
)

// A KeyDir is a key directory, opened without following a symlink and
// checked (6a-M1): a real directory, owned by one of its owners, with no
// group or other access. Every file in it is opened relative to it, never
// following a symlink either, and checked the same way before it is used.
// So a name swapped under it can't make the caller read a planted key or
// write through a link, even when the caller is root and the directory's
// parent belongs to someone else. Something that fails a check is refused,
// never repaired.
type KeyDir struct {
	f      *os.File
	path   string
	owners []int
}

// OpenKeyDir opens path as a KeyDir. If create is set and path doesn't
// exist, it is made 0700 first. Only the last part of path is opened
// without following symlinks; the directories above it must be ones the
// caller trusts.
func OpenKeyDir(path string, create bool, owners ...int) (*KeyDir, error) {
	if create {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, refused(path, err)
	}
	return newKeyDir(fd, path, owners)
}

// Sub opens the directory name inside d as a KeyDir, as OpenKeyDir does.
func (d *KeyDir) Sub(name string, create bool, owners ...int) (*KeyDir, error) {
	path := filepath.Join(d.path, name)
	if create {
		if err := unix.Mkdirat(d.fd(), name, 0o700); err != nil && err != unix.EEXIST {
			return nil, &os.PathError{Op: "mkdir", Path: path, Err: err}
		}
	}
	fd, err := unix.Openat(d.fd(), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, refused(path, err)
	}
	return newKeyDir(fd, path, owners)
}

func newKeyDir(fd int, path string, owners []int) (*KeyDir, error) {
	f := os.NewFile(uintptr(fd), path)
	if err := check(f, path, true, owners, 0o077); err != nil {
		f.Close()
		return nil, err
	}
	return &KeyDir{f: f, path: path, owners: owners}, nil
}

func (d *KeyDir) fd() int      { return int(d.f.Fd()) }
func (d *KeyDir) Path() string { return d.path }
func (d *KeyDir) Close() error { return d.f.Close() }

// refused names the usual reasons an open without following symlinks
// fails.
func refused(path string, err error) error {
	if err == unix.ELOOP || err == unix.ENOTDIR {
		return fmt.Errorf("%s: not a real directory or file (a symlink?); refusing to use it", path)
	}
	return &os.PathError{Op: "open", Path: path, Err: err}
}

// check is the KeyDir rule for an open file: the right type, owned by
// one of owners, and none of the mode bits in deny.
func check(f *os.File, path string, dir bool, owners []int, deny uint32) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return &os.PathError{Op: "fstat", Path: path, Err: err}
	}
	want, what := uint32(unix.S_IFREG), "regular file"
	if dir {
		want, what = unix.S_IFDIR, "directory"
	}
	if st.Mode&unix.S_IFMT != want {
		return fmt.Errorf("%s: not a %s; refusing to use it", path, what)
	}
	if !slices.Contains(owners, int(st.Uid)) {
		return fmt.Errorf("%s: owned by uid %d, which may not hold this key; refusing to use it", path, st.Uid)
	}
	if st.Mode&deny != 0 {
		return fmt.Errorf("%s: mode %04o is too open; refusing to use it", path, st.Mode&0o7777)
	}
	return nil
}

// The key may be read only by its owner; the certificate is public but
// may not be writable by anyone else.
const (
	keyDeny  = 0o077
	certDeny = 0o022
)

// open opens name in d for reading and checks it.
func (d *KeyDir) open(name string, deny uint32) (*os.File, error) {
	path := filepath.Join(d.path, name)
	fd, err := unix.Openat(d.fd(), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if err == unix.ENOENT {
			return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
		}
		return nil, refused(path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	if err := check(f, path, false, d.owners, deny); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (d *KeyDir) read(name string, deny uint32) ([]byte, error) {
	f, err := d.open(name, deny)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 64<<10))
}

// Load reads d's key and certificate. The key must be P-256 and match
// the certificate.
func (d *KeyDir) Load() (tls.Certificate, error) {
	keyPEM, err := d.read(KeyFile, keyDeny)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM, err := d.read(CertFile, certDeny)
	if err != nil {
		return tls.Certificate{}, err
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("%s: %w", d.path, err)
	}
	if len(c.Certificate) != 1 || !isP256(c.Leaf) {
		return tls.Certificate{}, errors.New("the key must be one P-256 certificate")
	}
	return c, nil
}

// exists reports whether name is in d, without following it.
func (d *KeyDir) exists(name string) (bool, error) {
	var st unix.Stat_t
	switch err := unix.Fstatat(d.fd(), name, &st, unix.AT_SYMLINK_NOFOLLOW); err {
	case nil:
		return true, nil
	case unix.ENOENT:
		return false, nil
	default:
		return false, &os.PathError{Op: "lstat", Path: filepath.Join(d.path, name), Err: err}
	}
}

// LoadOrCreate loads d's key, or makes one if neither file exists. Only
// one of the two there is an error, never a silent new key: a new key is
// a new identity. The key is written 0600 and the certificate 0644, each
// to a new temporary file that is then renamed.
func (d *KeyDir) LoadOrCreate() (tls.Certificate, error) {
	k, err := d.exists(KeyFile)
	if err != nil {
		return tls.Certificate{}, err
	}
	c, err := d.exists(CertFile)
	if err != nil {
		return tls.Certificate{}, err
	}
	switch {
	case k && c:
		return d.Load()
	case k || c:
		return tls.Certificate{}, fmt.Errorf("%s: only one of %s and %s exists; refusing to make a new key", d.path, KeyFile, CertFile)
	}
	cert, err := Generate()
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := d.Write(cert); err != nil {
		return tls.Certificate{}, err
	}
	return cert, nil
}

// Write stores c in d as LoadOrCreate describes.
func (d *KeyDir) Write(c tls.Certificate) error {
	keyPEM, certPEM, err := Encode(c)
	if err != nil {
		return err
	}
	if err := d.writeFile(KeyFile, keyPEM, 0o600); err != nil {
		return err
	}
	if err := d.writeFile(CertFile, certPEM, 0o644); err != nil {
		return err
	}
	return d.f.Sync()
}

// writeFile writes data to a new file in d (O_CREAT|O_EXCL, never
// following a link), syncs it and renames it to name.
func (d *KeyDir) writeFile(name string, data []byte, mode uint32) error {
	tmp := "." + name + ".new"
	// A leftover from a crash is d's own (d is checked), so it can go.
	if err := unix.Unlinkat(d.fd(), tmp, 0); err != nil && err != unix.ENOENT {
		return &os.PathError{Op: "remove", Path: filepath.Join(d.path, tmp), Err: err}
	}
	fd, err := unix.Openat(d.fd(), tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil {
		return &os.PathError{Op: "create", Path: filepath.Join(d.path, tmp), Err: err}
	}
	f := os.NewFile(uintptr(fd), filepath.Join(d.path, tmp))
	err = f.Chmod(os.FileMode(mode)) // the umask may have taken bits
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = unix.Renameat(d.fd(), tmp, d.fd(), name)
	}
	if err != nil {
		unix.Unlinkat(d.fd(), tmp, 0)
		return fmt.Errorf("writing %s: %w", filepath.Join(d.path, name), err)
	}
	return nil
}

// MoveTo moves d's key and certificate into dst and gives them to
// uid:gid. Each file is opened and checked in d first, and its owner is
// changed through that open file after the move, so whatever name dst
// holds by then, only these files change hands.
func (d *KeyDir) MoveTo(dst *KeyDir, uid, gid int) error {
	for _, n := range []struct {
		name string
		deny uint32
	}{{KeyFile, keyDeny}, {CertFile, certDeny}} {
		f, err := d.open(n.name, n.deny)
		if err != nil {
			return err
		}
		err = unix.Renameat(d.fd(), n.name, dst.fd(), n.name)
		if err == nil {
			err = f.Chown(uid, gid)
		}
		f.Close()
		if err != nil {
			return fmt.Errorf("moving %s to %s: %w", n.name, dst.path, err)
		}
	}
	return dst.f.Sync()
}

// RemoveSub removes the empty directory name inside d.
func (d *KeyDir) RemoveSub(name string) error {
	if err := unix.Unlinkat(d.fd(), name, unix.AT_REMOVEDIR); err != nil {
		return &os.PathError{Op: "remove", Path: filepath.Join(d.path, name), Err: err}
	}
	return nil
}

// Chown gives the directory itself to uid:gid, through the open
// directory.
func (d *KeyDir) Chown(uid, gid int) error { return d.f.Chown(uid, gid) }
