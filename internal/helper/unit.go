package helper

import (
	"bytes"
	_ "embed"
	"html/template"
)

// UnitMarker is the first line of every unit file Waystone writes.
// Provisioning only ever replaces a unit file that starts with it.
const UnitMarker = "# Managed by Waystone. Do not edit: provisioning rewrites this file.\n"

// unitTemplate is the game server's unit. It is part of the security
// review: every value in it is typed and comes from the helper, never
// text from the agent.
//
//go:embed unit.tmpl
var unitTemplate string

// UnitParams fills unit.tmpl.
type UnitParams struct {
	ServerID     string
	User         string // also the group: useradd --user-group gives each user its own
	Java         string
	HeapMiB      uint32
	MemoryMaxMiB uint32
}

// RenderUnit fills unit.tmpl with p.
//   - Parse unitTemplate with text/template. Use template.Must or return
//     the parse error; either way a broken template must not go unnoticed.
//   - A field the template names but UnitParams lacks is an Execute error,
//     so return Execute's error rather than ignoring it.
//   - Execute into a bytes.Buffer and return its bytes.
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
