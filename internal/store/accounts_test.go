// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// Stage 5a-1 unit tests; no database.

func TestValidateActorUser(t *testing.T) {
	const id = "0199e1a0-0000-7000-8000-0000000000f1"
	if err := ValidateActor(Actor{Type: ActorUser, ID: id, Name: "Trevor"}); err != nil {
		t.Errorf("ValidateActor(user) = %v, want nil", err)
	}
	for _, a := range []Actor{
		{Type: ActorUser, ID: "1000", Name: "x"},
		{Type: ActorUser, ID: strings.ToUpper(id), Name: "x"},
		{Type: ActorUser, ID: "", Name: "x"},
		{Type: ActorUser, ID: "trevor", Name: "x"},
		{Type: ActorUser, ID: id, Name: ""},
		{Type: "web_user", ID: id, Name: "x"},
	} {
		if err := ValidateActor(a); !errors.Is(err, ErrInvalidActor) {
			t.Errorf("ValidateActor(%+v) = %v, want ErrInvalidActor", a, err)
		}
	}
	if ActorUser != "user" {
		t.Errorf("ActorUser = %q, want \"user\" (Q2; it must match the CHECK in 00006)", ActorUser)
	}
}

func TestValidUsername(t *testing.T) {
	for _, n := range []string{"trevor", "a", "0", "t.r_e-v", "sharddevs01", strings.Repeat("a", 32)} {
		if !ValidUsername(n) {
			t.Errorf("ValidUsername(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"", "Trevor", ".trevor", "_t", "-t", "tre vor", "trévor", "a@b", strings.Repeat("a", 33), "t\n", "t/x"} {
		if ValidUsername(n) {
			t.Errorf("ValidUsername(%q) = true, want false", n)
		}
	}
}

func TestCleanUserAgent(t *testing.T) {
	ff := "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0"
	if got := CleanUserAgent(ff); got != ff {
		t.Errorf("CleanUserAgent(firefox) = %q", got)
	}
	for _, bad := range []string{"a\nb", "a\x00b", "a\tb", "\xff\xfe", "a\x7fb"} {
		if got := CleanUserAgent(bad); got != "" {
			t.Errorf("CleanUserAgent(%q) = %q, want empty", bad, got)
		}
	}
	long := strings.Repeat("a", 300)
	if got := CleanUserAgent(long); got != long[:200] {
		t.Errorf("CleanUserAgent(300 bytes) has %d bytes, want 200", len(got))
	}
	// Cut on a character boundary: 199 ASCII bytes then a 2-byte rune.
	edge := strings.Repeat("a", 199) + "é" + "tail"
	got := CleanUserAgent(edge)
	if len(got) > 200 || !utf8.ValidString(got) || got != strings.Repeat("a", 199) {
		t.Errorf("CleanUserAgent(rune across 200) = %d bytes, valid=%v", len(got), utf8.ValidString(got))
	}
	exact := strings.Repeat("a", 198) + "é"
	if got := CleanUserAgent(exact); got != exact {
		t.Errorf("CleanUserAgent(exactly 200 bytes) changed it")
	}
}

func TestPreferencesValid(t *testing.T) {
	if !DefaultPreferences.Valid() || DefaultPreferences != (Preferences{Theme: "stone", Density: "comfortable", Mode: "dark"}) {
		t.Errorf("DefaultPreferences = %+v", DefaultPreferences)
	}
	for _, th := range []string{"stone", "slate"} {
		for _, d := range []string{"comfortable", "compact"} {
			for _, m := range []string{"dark", "light", "system"} {
				if p := (Preferences{th, d, m}); !p.Valid() {
					t.Errorf("%+v invalid", p)
				}
			}
		}
	}
	for _, p := range []Preferences{{}, {"Stone", "compact", "dark"}, {"stone", "", "dark"}, {"stone", "compact", "auto"}} {
		if p.Valid() {
			t.Errorf("%+v valid", p)
		}
	}
}

// The methods that need a browser user check that first, so a nil
// transaction is never touched.
func TestUserMethodsNeedUser(t *testing.T) {
	tx := &Tx{actor: Actor{Type: ActorLocalUser, ID: "1000", Name: "x"}}
	ctx := t.Context()
	if _, err := tx.CreateSession(ctx, NewSession{}); !errors.Is(err, ErrNotUser) {
		t.Errorf("CreateSession = %v", err)
	}
	if err := tx.TouchSession(ctx, ""); !errors.Is(err, ErrNotUser) {
		t.Errorf("TouchSession = %v", err)
	}
	if err := tx.DeleteSession(ctx, ""); !errors.Is(err, ErrNotUser) {
		t.Errorf("DeleteSession = %v", err)
	}
	if _, err := tx.Preferences(ctx); !errors.Is(err, ErrNotUser) {
		t.Errorf("Preferences = %v", err)
	}
	if err := tx.SetPreferences(ctx, DefaultPreferences); !errors.Is(err, ErrNotUser) {
		t.Errorf("SetPreferences = %v", err)
	}
}

// The 00006 migration is embedded.
func TestMigration00006Embedded(t *testing.T) {
	if _, err := Migrations().Open("00006_browser_accounts.sql"); err != nil {
		t.Fatalf("00006_browser_accounts.sql not embedded: %v", err)
	}
}
