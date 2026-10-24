// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/serverlog"
)

// Stage 7a (notes/stage7-scope.md D1): ACTION_READ_LOG reads a server's
// lines from journald. The game's stdout and stderr already go there.
// The agent isn't in systemd-journal or adm, so it can't read the whole
// journal itself; the helper reads only the one unit it derives from the
// server ID.

// Journalctl is the absolute path of journalctl. We never look it up in
// PATH.
const Journalctl = "/usr/bin/journalctl"

// journalReadBytes caps what one read takes from journalctl. Without
// --all, journald prints a field over 4096 bytes as null, so one entry
// is at most about 25 KiB of JSON and this always holds dozens.
const journalReadBytes = 1 << 20

// journalTimeout is how long one journalctl may run before it is killed
// (audit 7M2).
const journalTimeout = 5 * time.Second

// MaxLogReads is how many READ_LOG calls the helper runs at once; one
// more is refused at once with ERROR_CODE_CONFLICT (audit H1).
const MaxLogReads = 4

// logReads is the helper process's own count of READ_LOG calls.
var logReads = make(chan struct{}, MaxLogReads)

// tooLong stands in for a line journald won't print because it is over
// 4096 bytes.
const tooLong = "(a line too long to show)"

// LogCommand is a LogSpec that passed ValidateLogSpec.
type LogCommand struct {
	AfterCursor string
	Limit       int
}

// ValidateLogSpec checks a READ_LOG request's spec.
func ValidateLogSpec(spec *helperv1.LogSpec) (LogCommand, error) {
	if spec == nil {
		return LogCommand{}, fmt.Errorf("%w: missing log spec", ErrInvalidRequest)
	}
	if spec.GetLimit() < 1 || spec.GetLimit() > serverlog.MaxLines {
		return LogCommand{}, fmt.Errorf("%w: log limit out of range", ErrInvalidRequest)
	}
	if c := spec.GetAfterCursor(); c != "" && !serverlog.ValidCursor(c) {
		return LogCommand{}, fmt.Errorf("%w: invalid log cursor", ErrInvalidRequest)
	}
	return LogCommand{AfterCursor: spec.GetAfterCursor(), Limit: int(spec.GetLimit())}, nil
}

// CappedRunner runs a program like Runner does, but takes at most max
// bytes of its output. capped says it stopped there; the program is then
// killed, and what it printed so far is returned without an error.
type CappedRunner interface {
	RunCapped(ctx context.Context, argv []string, max int) (out []byte, capped bool, err error)
}

// Journal reads server logs with journalctl.
type Journal struct {
	Runner CappedRunner
	// Reads limits the READ_LOG calls running at once. nil means the one
	// shared by the whole helper process (logReads).
	Reads chan struct{}
}

// ErrJournal: journalctl printed something that isn't what it prints.
var ErrJournal = errors.New("unexpected journalctl output")

// ErrTooManyReads: MaxLogReads are already running. Nothing was run.
var ErrTooManyReads = errors.New("too many log reads at once")

// argv is journalctl's whole command line. Only the cursor and the count
// come from the request, each as its own argument.
//
// Without a cursor it reads the newest lines, newest first (--reverse),
// so a capped read loses the oldest ones. After a cursor it reads forward
// with no count: journalctl stops when the output is capped, which is
// the same on every systemd version, whatever --lines does with a cursor.
func (j Journal) argv(serverID string, cmd LogCommand) []string {
	argv := []string{
		Journalctl,
		"--no-pager",
		"--quiet",
		"--output=json",
		"--output-fields=MESSAGE,PRIORITY",
		"--unit=" + UnitName(serverID),
	}
	if cmd.AfterCursor == "" {
		return append(argv, "--reverse", "--lines="+strconv.Itoa(cmd.Limit))
	}
	return append(argv, "--after-cursor="+cmd.AfterCursor)
}

// Read runs a READ_LOG command. ValidateRequest has checked it already;
// it is checked again here, right before anything runs as root, so no
// path to Read can skip it (audit 7M2).
func (j Journal) Read(ctx context.Context, cmd Command) (*helperv1.LogResult, error) {
	if ids.Check(cmd.ServerID) != nil || cmd.Log.Limit < 1 || cmd.Log.Limit > serverlog.MaxLines ||
		(cmd.Log.AfterCursor != "" && !serverlog.ValidCursor(cmd.Log.AfterCursor)) {
		return nil, fmt.Errorf("%w: bad log read", ErrInvalidRequest)
	}
	reads := j.Reads
	if reads == nil {
		reads = logReads
	}
	select {
	case reads <- struct{}{}:
		defer func() { <-reads }()
	default:
		return nil, ErrTooManyReads
	}
	ctx, cancel := context.WithTimeout(ctx, journalTimeout)
	defer cancel()
	out, capped, err := j.Runner.RunCapped(ctx, j.argv(cmd.ServerID, cmd.Log), journalReadBytes)
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(out, []byte("\n"))
	// The last piece is empty after a final newline, or half an entry
	// when the output was capped. Either way it isn't an entry.
	lines = lines[:len(lines)-1]
	entries := make([]journalEntry, 0, len(lines))
	for _, l := range lines {
		e, err := parseEntry(l)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}

	if cmd.Log.AfterCursor == "" {
		// Newest first: keep the newest that fit, then put them in order.
		kept := pack(entries, cmd.Log.Limit)
		slices.Reverse(kept)
		return result(kept, false), nil
	}
	if capped && len(entries) == 0 {
		return nil, fmt.Errorf("%w: no whole entry in %d bytes", ErrJournal, journalReadBytes)
	}
	kept := pack(entries, cmd.Log.Limit)
	return result(kept, capped || len(kept) < len(entries)), nil
}

// pack takes entries from the front while there's room: at most limit,
// and MaxTextBytes of text in all.
func pack(entries []journalEntry, limit int) []journalEntry {
	size := 0
	for i, e := range entries {
		size += len(e.line.Text)
		if i == limit || size > serverlog.MaxTextBytes {
			return entries[:i]
		}
	}
	return entries
}

func result(entries []journalEntry, more bool) *helperv1.LogResult {
	r := &helperv1.LogResult{More: more}
	for _, e := range entries {
		r.Lines = append(r.Lines, &helperv1.LogLine{
			TimeUnixMicro: e.line.Time.UnixMicro(),
			Priority:      e.line.Priority,
			Text:          e.line.Text,
		})
	}
	if len(entries) > 0 {
		r.Cursor = entries[len(entries)-1].cursor
	}
	return r
}

type journalEntry struct {
	line   serverlog.Line
	cursor string
}

// parseEntry reads one line of journalctl's JSON. A field journald holds
// more than once is an array; MESSAGE can also be an array of bytes
// (when it isn't UTF-8) or null (over 4096 bytes).
func parseEntry(raw []byte) (journalEntry, error) {
	var fields struct {
		Cursor   string          `json:"__CURSOR"`
		Realtime string          `json:"__REALTIME_TIMESTAMP"`
		Priority json.RawMessage `json:"PRIORITY"`
		Message  json.RawMessage `json:"MESSAGE"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return journalEntry{}, fmt.Errorf("%w: %v", ErrJournal, err)
	}
	if !serverlog.ValidCursor(fields.Cursor) {
		return journalEntry{}, fmt.Errorf("%w: bad cursor", ErrJournal)
	}
	us, err := strconv.ParseInt(fields.Realtime, 10, 64)
	if err != nil || us < 0 {
		return journalEntry{}, fmt.Errorf("%w: bad timestamp", ErrJournal)
	}
	return journalEntry{
		line: serverlog.Line{
			Time:     time.UnixMicro(us).UTC(),
			Priority: priority(fields.Priority),
			Text:     serverlog.Clean(message(fields.Message)),
		},
		cursor: fields.Cursor,
	}, nil
}

// message is MESSAGE as text: a string, an array of bytes, or tooLong
// for null. Anything else (an entry with several) is empty.
func message(raw json.RawMessage) string {
	// Unmarshal takes null for any type without an error, so look first.
	if string(raw) == "null" {
		return tooLong
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var nums []int
	if json.Unmarshal(raw, &nums) != nil {
		return ""
	}
	b := make([]byte, 0, len(nums))
	for _, n := range nums {
		if n < 0 || n > 255 {
			return ""
		}
		b = append(b, byte(n))
	}
	return string(b)
}

// priority is PRIORITY as a number, info (6) when it's missing or odd.
func priority(raw json.RawMessage) uint32 {
	var s string
	if json.Unmarshal(raw, &s) == nil && len(s) == 1 && s[0] >= '0' && s[0] <= '7' {
		return uint32(s[0] - '0')
	}
	return 6
}
