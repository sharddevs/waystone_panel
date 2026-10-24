// SPDX-License-Identifier: AGPL-3.0-only

package view

import (
	"time"

	"github.com/sharddevs/waystone_panel/internal/serverlog"
)

// LogLine is one line of a server's log (7a).
type LogLine struct {
	Time time.Time `json:"time"`
	// Priority is syslog's 0 (emerg) to 7 (debug).
	Priority uint32 `json:"priority"`
	// Text is plain text, already cleaned: no control characters but tab,
	// no bidi controls, at most 4 KiB.
	Text string `json:"text"`
}

// LogPage is one read of a server's log, oldest line first. Cursor is
// where the next read continues (pass it as ?after=), null before the
// server has logged anything. More says lines already follow it.
type LogPage struct {
	Lines  []LogLine `json:"lines"`
	Cursor *string   `json:"cursor"`
	More   bool      `json:"more"`
}

// LogPageOf is p's public shape.
func LogPageOf(p serverlog.Page) LogPage {
	out := LogPage{Lines: make([]LogLine, 0, len(p.Lines)), More: p.More}
	for _, l := range p.Lines {
		out.Lines = append(out.Lines, LogLine{Time: l.Time.UTC(), Priority: l.Priority, Text: l.Text})
	}
	if p.Cursor != "" {
		c := p.Cursor
		out.Cursor = &c
	}
	return out
}
