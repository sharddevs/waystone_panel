//go:build devharness

package main

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func TestReadFileDoesNotDispatchMutations(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}

	file, err := parser.ParseFile(token.NewFileSet(), "main.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}

	// This is a temporary structural guard until command dispatch
	// is extracted into a separately testable function.
	_ = file

	text := string(source)
	start := strings.Index(text, `case "read-file":`)
	end := strings.Index(text, `case "start":`)

	if start < 0 || end <= start {
		t.Fatal("could not locate read-file case boundaries")
	}

	body := text[start:end]

	if !strings.Contains(body, "runtime.ReadFile(") {
		t.Fatal("read-file does not call runtime.ReadFile")
	}

	for _, forbidden := range []string{
		"minecraft.AcceptEula(",
		"minecraft.WriteProperties(",
		"runtime.Provision(",
		"runtime.Start(",
		"runtime.Stop(",
		"runtime.WriteFile(",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("read-file unexpectedly contains %s", forbidden)
		}
	}
}
