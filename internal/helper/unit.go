package helper

import (
	"bytes"
	_ "embed"
	"html/template"
)

// UnitMarker is the first line of every unit file Waystone writes.
// Provisioning only ever replaces a unit file that starts with it.
const UnitMarker = "# Managed by Waystone. Do not edit: provisioning rewrites this file.\n"

// unitTemplate is the game server's unit. Every value filled into it comes
// from the helper as a typed field, never as text from the agent.
//
//go:embed unit.tmpl
var unitTemplate string

// consoleTemplate is the server's console socket (7b): the FIFO that is
// the game's stdin.
//
//go:embed console.tmpl
var consoleTemplate string

// ConsoleGroup is the group that may write a console FIFO: the agent's.
const ConsoleGroup = "waystone-agent"

// ConsoleSocketName returns the console socket unit for a server,
// "waystone-srv-<id>-console.socket". serverID has to be validated already.
func ConsoleSocketName(serverID string) string {
	return "waystone-srv-" + serverID + "-console.socket"
}

// UnitParams holds the values filled into unit.tmpl.
type UnitParams struct {
	ServerID     string
	User         string // also the group name: useradd --user-group gives each user its own
	Java         string
	HeapMiB      uint32
	MemoryMaxMiB uint32
}

// RenderUnit fills unit.tmpl with p. A field the template names but
// UnitParams lacks shows up as an Execute error, so we return it.
func RenderUnit(p UnitParams) ([]byte, error) {
	return render(unitTemplate, p)
}

// RenderConsoleSocket fills console.tmpl for serverID.
func RenderConsoleSocket(serverID string) ([]byte, error) {
	return render(consoleTemplate, struct{ ServerID, Group string }{serverID, ConsoleGroup})
}

func render(text string, data any) ([]byte, error) {
	tmpl, err := template.New("waystone-unit").Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
