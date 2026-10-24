// SPDX-License-Identifier: AGPL-3.0-only

package minecraft

import (
	"errors"
	"strings"
	"testing"
)

var testProps = Properties{ServerPort: 25566, Motd: "Waystone test", MaxPlayers: 20}

// The managed lines for testProps, in append order: the four settings,
// then the fixed ones (6H5).
const (
	locked  = "server-ip=\nenable-rcon=false\nenable-query=false\nmanagement-server-enabled=false\nenable-jmx-monitoring=false\n"
	managed = "server-port=25566\nmotd=Waystone test\nmax-players=20\nonline-mode=true\n" + locked
)

func TestValidateAccepts(t *testing.T) {
	for _, p := range []Properties{
		testProps,
		{ServerPort: 1024, Motd: "", MaxPlayers: 1},
		{ServerPort: 65535, Motd: strings.Repeat("x", 64), MaxPlayers: 1000},
		{ServerPort: 25565, Motd: "Hi! #1 = :) ~ {ok}", MaxPlayers: 20},
		{ServerPort: 25565, Motd: "inner  spaces", MaxPlayers: 20},
	} {
		if err := p.Validate(); err != nil {
			t.Errorf("Validate(%+v): %v", p, err)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	tests := map[string]Properties{
		"port 0":          {ServerPort: 0, Motd: "m", MaxPlayers: 20},
		"port 1023":       {ServerPort: 1023, Motd: "m", MaxPlayers: 20},
		"port 65536":      {ServerPort: 65536, Motd: "m", MaxPlayers: 20},
		"players 0":       {ServerPort: 25565, Motd: "m", MaxPlayers: 0},
		"players 1001":    {ServerPort: 25565, Motd: "m", MaxPlayers: 1001},
		"motd 65 bytes":   {ServerPort: 25565, Motd: strings.Repeat("x", 65), MaxPlayers: 20},
		"motd backslash":  {ServerPort: 25565, Motd: `a\nb`, MaxPlayers: 20},
		"motd newline":    {ServerPort: 25565, Motd: "a\nb", MaxPlayers: 20},
		"motd tab":        {ServerPort: 25565, Motd: "a\tb", MaxPlayers: 20},
		"motd DEL":        {ServerPort: 25565, Motd: "a\x7fb", MaxPlayers: 20},
		"motd non-ASCII":  {ServerPort: 25565, Motd: "caf\u00e9", MaxPlayers: 20},
		"motd leading sp": {ServerPort: 25565, Motd: " a", MaxPlayers: 20},
		"motd trailing":   {ServerPort: 25565, Motd: "a ", MaxPlayers: 20},
	}
	for name, p := range tests {
		if err := p.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestMergeProperties(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"no file", "", managed},
		{
			"typical Minecraft file: replaced in place, others kept",
			"#Minecraft server properties\n#Sat Oct 10 23:59:11 UTC 2026\ndifficulty=easy\nmax-players=10\nmotd=A Minecraft Server\nonline-mode=false\nserver-port=25565\nwhite-list=true\n",
			"#Minecraft server properties\n#Sat Oct 10 23:59:11 UTC 2026\ndifficulty=easy\nmax-players=20\nmotd=Waystone test\nonline-mode=true\nserver-port=25566\nwhite-list=true\n" + locked,
		},
		{
			"duplicates: first replaced, later ones removed, missing appended",
			"server-port=25565\nserver-port=25566\nonline-mode=false\n",
			"server-port=25566\nonline-mode=true\nmotd=Waystone test\nmax-players=20\n" + locked,
		},
		{
			"other separators and leading whitespace",
			"  server-port : 1\nmotd\thello\nmax-players:5\nonline-mode\n",
			managed,
		},
		{
			"commented-out keys are comments",
			"#server-port=1\n!motd=x\n",
			"#server-port=1\n!motd=x\n" + managed,
		},
		{
			"a continued value is never a key line",
			"description=line one \\\n  server-port=9\nlevel-name=world\n",
			"description=line one \\\n  server-port=9\nlevel-name=world\n" + managed,
		},
		{
			"an even number of backslashes does not continue",
			"path=C:\\\\\nserver-port=1\n",
			"path=C:\\\\\nserver-port=25566\nmotd=Waystone test\nmax-players=20\nonline-mode=true\n" + locked,
		},
		{
			"comment lines never continue",
			"#note \\\nserver-port=1\n",
			"#note \\\nserver-port=25566\nmotd=Waystone test\nmax-players=20\nonline-mode=true\n" + locked,
		},
		{
			"unrelated malformed lines are kept",
			"=novalue\n:x\nweird line without separator\n",
			"=novalue\n:x\nweird line without separator\n" + managed,
		},
		{
			"blank and whitespace-only lines are kept",
			"\n   \nlevel-name=w\n",
			"\n   \nlevel-name=w\n" + managed,
		},
		{
			"CRLF lines: managed line rewritten, others untouched",
			"server-port=1\r\nlevel-name=world\r\n",
			"server-port=25566\nlevel-name=world\r\nmotd=Waystone test\nmax-players=20\nonline-mode=true\n" + locked,
		},
		{
			"the fixed keys are rewritten in place (6H5)",
			"enable-rcon=true\nrcon.password=hunter2\nserver-ip=10.0.0.1\nenable-query=true\nmanagement-server-enabled=true\nenable-jmx-monitoring=true\n",
			"enable-rcon=false\nrcon.password=hunter2\nserver-ip=\nenable-query=false\nmanagement-server-enabled=false\nenable-jmx-monitoring=false\n" +
				"server-port=25566\nmotd=Waystone test\nmax-players=20\nonline-mode=true\n",
		},
		{
			"no final newline",
			"level-name=world",
			"level-name=world\n" + managed,
		},
	}
	for _, tt := range tests {
		got, err := MergeProperties([]byte(tt.in), testProps)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if string(got) != tt.want {
			t.Errorf("%s:\ngot  %q\nwant %q", tt.name, got, tt.want)
		}
	}
}

func TestMergePropertiesNil(t *testing.T) {
	got, err := MergeProperties(nil, testProps)
	if err != nil || string(got) != managed {
		t.Errorf("MergeProperties(nil) = %q, %v; want %q", got, err, managed)
	}
}

// Running it twice changes nothing the second time.
func TestMergePropertiesIsIdempotent(t *testing.T) {
	in := "#c\nwhite-list=true\nserver-port=1\nserver-port=2\n"
	once, err := MergeProperties([]byte(in), testProps)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := MergeProperties(once, testProps)
	if err != nil {
		t.Fatal(err)
	}
	if string(once) != string(twice) {
		t.Errorf("second merge changed the file:\n%q\n%q", once, twice)
	}
}

func TestMergePropertiesRefuses(t *testing.T) {
	for name, in := range map[string]string{
		"escaped managed key":        "server\\-port=1\n",
		"any escaped key":            "my\\ key=1\n",
		"managed key that continues": "motd=hello \\\n world\n",
	} {
		got, err := MergeProperties([]byte(in), testProps)
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: err = %v, want ErrUnsupported", name, err)
		}
		if got != nil {
			t.Errorf("%s: returned output %q along with an error", name, got)
		}
	}
}
func TestMergePropertiesEmptyKeyContinuation(t *testing.T) {
	input := "=x\\\nserver-port=1\n"

	props := Properties{
		ServerPort: 25565,
		Motd:       "Waystone test",
		MaxPlayers: 20,
	}

	got, err := MergeProperties([]byte(input), props)
	if err != nil {
		t.Fatal(err)
	}

	want := "=x\\\nserver-port=1\n" +
		"server-port=25565\n" +
		"motd=Waystone test\n" +
		"max-players=20\n" +
		"online-mode=true\n" + locked

	if string(got) != want {
		t.Errorf("MergeProperties:\ngot:\n%s\nwant:\n%s", got, want)
	}
}
