package helper

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

var goldenParams = UnitParams{
	ServerID:     testServerID,
	User:         "ws-16362f566387",
	Java:         "/usr/lib/jvm/java-25-openjdk-amd64/bin/java",
	HeapMiB:      2048,
	MemoryMaxMiB: 3072,
}

// The rendered unit is pinned byte for byte: any change to the game
// server's sandbox shows up as a diff of testdata/waystone-srv.service.
func TestRenderUnitGolden(t *testing.T) {
	got, err := RenderUnit(goldenParams)
	if err != nil {
		t.Fatalf("RenderUnit: %v", err)
	}
	want, err := os.ReadFile("testdata/waystone-srv.service")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("rendered unit differs from testdata/waystone-srv.service:\n%s", got)
	}
}

func TestRenderUnitStartsWithMarker(t *testing.T) {
	got, err := RenderUnit(goldenParams)
	if err != nil {
		t.Fatalf("RenderUnit: %v", err)
	}
	if !strings.HasPrefix(string(got), UnitMarker) {
		t.Errorf("unit does not start with UnitMarker:\n%s", got)
	}
}
