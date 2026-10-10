package ids

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestCheckAccepts(t *testing.T) {
	for _, s := range []string{
		"0192a5c4-7d3e-7b1a-9c2f-3e4d5f6a7b8c",
		"00000000-0000-0000-0000-000000000000",
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
		"3f2504e0-4f89-41d3-9a0c-0305e82c3301",
	} {
		if err := Check(s); err != nil {
			t.Errorf("Check(%q) = %v, want nil", s, err)
		}
	}
}

func TestCheckRejects(t *testing.T) {
	for _, s := range []string{
		"",
		"0192A5C4-7D3E-7B1A-9C2F-3E4D5F6A7B8C",   // uppercase
		"{0192a5c4-7d3e-7b1a-9c2f-3e4d5f6a7b8c}", // braces
		"urn:uuid:0192a5c4-7d3e-7b1a-9c2f-3e4d5f6a7b8c", // URN form
		"0192a5c47d3e7b1a9c2f3e4d5f6a7b8c",              // no dashes
		"0192a5c4-7d3e-7b1a-9c2f-3e4d5f6a7b8",           // too short
		"0192a5c4-7d3e-7b1a-9c2f-3e4d5f6a7b8c0",         // too long
		"0192a5c47-d3e-7b1a-9c2f-3e4d5f6a7b8c",          // dash in the wrong place
		"0192a5c4-7d3e-7b1a-9c2f-3e4d5f6a7b8g",          // not hex
		"0192a5c4-7d3e-7b1a-9c2f-3e4d5f6a7b8c\n",        // trailing newline
		" 0192a5c4-7d3e-7b1a-9c2f-3e4d5f6a7b8c",         // leading space
		"../../../../etc/passwd-aaaa-aaaa-aaaaaaaaaa",   // path tricks
	} {
		if err := Check(s); !errors.Is(err, ErrInvalid) {
			t.Errorf("Check(%q) = %v, want ErrInvalid", s, err)
		}
	}
}

func TestNewV7(t *testing.T) {
	before := time.Now().UnixMilli()
	id, err := NewV7()
	after := time.Now().UnixMilli()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	if err := Check(id); err != nil {
		t.Fatalf("NewV7 returned %q, which fails Check: %v", id, err)
	}
	if id[14] != '7' {
		t.Errorf("version digit = %q, want '7' (id %s)", id[14], id)
	}
	switch id[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Errorf("variant digit = %q, want one of 8 9 a b (id %s)", id[19], id)
	}
	ms, err := strconv.ParseInt(id[0:8]+id[9:13], 16, 64)
	if err != nil {
		t.Fatalf("timestamp not hex: %v", err)
	}
	if ms < before || ms > after {
		t.Errorf("timestamp %d not between %d and %d", ms, before, after)
	}

	other, err := NewV7()
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	if other == id {
		t.Errorf("two calls returned the same id %s", id)
	}
}
