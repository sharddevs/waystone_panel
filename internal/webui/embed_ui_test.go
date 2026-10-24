// SPDX-License-Identifier: AGPL-3.0-only

//go:build ui

package webui

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
)

// The real build, as embedded with -tags ui: index.html passes
// CheckIndex, every file it and the CSS name is in the build, the five
// fonts and their licence are there, and nothing has a type that
// wouldn't be served.
func TestEmbeddedBuild(t *testing.T) {
	files := Embedded()
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckIndex(index); err != nil {
		t.Fatal(err)
	}
	if _, err := Routes(files); err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`(?:src|href)="(/[^"]*)"`).FindAllSubmatch(index, -1)
	if len(refs) < 2 {
		t.Fatalf("index.html names %d files: %s", len(refs), index)
	}
	for _, m := range refs {
		if _, err := fs.Stat(files, strings.TrimPrefix(string(m[1]), "/")); err != nil {
			t.Errorf("index.html names %s: %v", m[1], err)
		}
	}
	fonts := 0
	err = fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if _, ok := types[path.Ext(p)]; !ok {
			t.Errorf("%s: a type that is never served", p)
		}
		if strings.HasSuffix(p, ".woff2") {
			fonts++
		}
		if strings.HasSuffix(p, ".css") {
			css, _ := fs.ReadFile(files, p)
			for _, u := range regexp.MustCompile(`url\(([^)]*)\)`).FindAllSubmatch(css, -1) {
				ref := strings.Trim(string(u[1]), `"'`)
				if strings.HasPrefix(ref, "data:") || strings.Contains(ref, "//") {
					t.Errorf("%s loads %s", p, ref)
					continue
				}
				if _, err := fs.Stat(files, strings.TrimPrefix(ref, "/")); err != nil {
					t.Errorf("%s names %s: %v", p, ref, err)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if fonts != 5 {
		t.Errorf("%d font files, want 5", fonts)
	}
	if b, err := fs.ReadFile(files, "licenses/OFL.txt"); err != nil || !strings.Contains(string(b), "SIL Open Font License") {
		t.Errorf("licenses/OFL.txt: %v", err)
	}
}
