// SPDX-License-Identifier: AGPL-3.0-only

package worker

import (
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func sum(s string) [sha256.Size]byte { return sha256.Sum256([]byte(s)) }

func TestMaxInstallBytes(t *testing.T) {
	if MaxInstallBytes != 256<<20 {
		t.Errorf("MaxInstallBytes = %d, want 256 MiB", MaxInstallBytes)
	}
}

func TestParseSHA256(t *testing.T) {
	want := sum("jar")
	hexed := ExpectContent([]byte("jar")).String()
	got, err := ParseSHA256(hexed)
	if err != nil || got != want {
		t.Errorf("ParseSHA256(%q) = %x, %v; want %x", hexed, got, err, want)
	}
	// Besides "" and "absent": uppercase, too short, too long, a non-hex
	// digit, and a leading space.
	for _, s := range []string{
		"", "absent", strings.ToUpper(hexed), hexed[:63], hexed + "0", hexed[:63] + "g", " " + hexed[1:],
	} {
		if _, err := ParseSHA256(s); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseSHA256(%q) = %v, want ErrInvalid", s, err)
		}
	}
}

func TestInstallFileCreatesAndReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.jar")
	// OneByteReader returns one byte per Read, which catches code that
	// assumes a single Read gets everything.
	n, err := InstallFile(dir, "server.jar", iotest.OneByteReader(strings.NewReader("first")), sum("first"))
	if err != nil || n != 5 {
		t.Fatalf("InstallFile new = %d, %v; want 5, nil", n, err)
	}
	if got := readFile(t, path); got != "first" {
		t.Errorf("content = %q, want first", got)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != FileMode {
		t.Errorf("mode = %v, want a regular file with %v", info.Mode(), FileMode)
	}

	// There's no expected hash for the old file: it's just replaced.
	if _, err := InstallFile(dir, "server.jar", strings.NewReader("second"), sum("second")); err != nil {
		t.Fatalf("InstallFile over an existing file: %v", err)
	}
	if got := readFile(t, path); got != "second" {
		t.Errorf("content = %q, want second", got)
	}
	noTemps(t, dir)
}

func TestInstallFileEmpty(t *testing.T) {
	dir := t.TempDir()
	if n, err := InstallFile(dir, "empty.bin", strings.NewReader(""), sum("")); err != nil || n != 0 {
		t.Fatalf("InstallFile empty = %d, %v; want 0, nil", n, err)
	}
	if got := readFile(t, filepath.Join(dir, "empty.bin")); got != "" {
		t.Errorf("content = %q, want empty", got)
	}
}

func TestInstallFileReplacesSymlinkWithoutFollowing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	writeFile(t, target, "keep")
	if err := os.Symlink("target.txt", filepath.Join(dir, "server.jar")); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallFile(dir, "server.jar", strings.NewReader("jar"), sum("jar")); err != nil {
		t.Fatalf("InstallFile over a symlink: %v", err)
	}
	if got := readFile(t, target); got != "keep" {
		t.Errorf("symlink target = %q, want keep: the link was followed", got)
	}
	info, err := os.Lstat(filepath.Join(dir, "server.jar"))
	if err != nil || !info.Mode().IsRegular() {
		t.Errorf("server.jar = %v, %v; want a regular file in place of the link", info, err)
	}
}

func TestInstallFileRefusesMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.jar")
	writeFile(t, path, "old")
	_, err := InstallFile(dir, "server.jar", strings.NewReader("evil"), sum("good"))
	if !errors.Is(err, ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch", err)
	}
	if got := readFile(t, path); got != "old" {
		t.Errorf("content = %q, want old: a mismatch must leave the file alone", got)
	}
	noTemps(t, dir)
}

func TestInstallFileSizeLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	exact := strings.Repeat("x", 10)
	if n, err := installFile(dir, "f.bin", strings.NewReader(exact), sum(exact), 10); err != nil || n != 10 {
		t.Fatalf("exactly the limit = %d, %v; want 10, nil", n, err)
	}

	// One byte too many is refused, even with the right hash for all of it.
	over := strings.Repeat("y", 11)
	_, err := installFile(dir, "f.bin", strings.NewReader(over), sum(over), 10)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("one byte over = %v, want ErrInvalid", err)
	}
	// The hash of the first ten bytes, which a silent truncation would
	// match, is refused too.
	_, err = installFile(dir, "f.bin", strings.NewReader(over), sum(over[:10]), 10)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("one byte over, truncated hash = %v, want ErrInvalid", err)
	}
	if got := readFile(t, path); got != exact {
		t.Errorf("content = %q, want the first install unchanged", got)
	}
	noTemps(t, dir)
}

func TestInstallFileStopsReadingAtTheLimit(t *testing.T) {
	// An endless stream has to be refused after limit+1 bytes instead of
	// being read until the worker is killed.
	r := &countingReader{r: neverEnding{}}
	_, err := installFile(t.TempDir(), "f.bin", r, sum(""), 10)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("endless stream = %v, want ErrInvalid", err)
	}
	if r.n > 11 {
		t.Errorf("read %d bytes of an endless stream with a limit of 10", r.n)
	}
}

type neverEnding struct{}

func (neverEnding) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'z'
	}
	return len(p), nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func TestInstallFileReadError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.jar")
	writeFile(t, path, "old")
	// The error comes after "part" was already written to the temp file.
	r := io.MultiReader(strings.NewReader("part"), iotest.ErrReader(errors.New("disk on fire")))
	if _, err := InstallFile(dir, "server.jar", r, sum("part")); err == nil {
		t.Error("a failing reader installed something")
	}
	if got := readFile(t, path); got != "old" {
		t.Errorf("content = %q, want old", got)
	}
	noTemps(t, dir)
}

func TestInstallFileRejectsBadNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"", ".hidden", "../x", "a/b", "-x"} {
		if _, err := InstallFile(dir, name, strings.NewReader("x"), sum("x")); !errors.Is(err, ErrInvalid) {
			t.Errorf("InstallFile(%q) = %v, want ErrInvalid", name, err)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("bad names created files: %v", entries)
	}
}
