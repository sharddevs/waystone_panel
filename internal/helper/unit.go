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
	tmpl, err := template.New("waystone-unit").Parse(unitTemplate)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, p); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
