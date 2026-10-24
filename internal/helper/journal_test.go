// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/serverlog"
)

const testCursor = "s=0123456789abcdef0123456789abcdef;i=1a2;b=fedcba9876543210fedcba9876543210;m=9f;t=5f1e2d3c4b5a6;x=77aa"

// entry is one line of journalctl's JSON output.
func entry(n int, message string) string {
	return fmt.Sprintf(`{"__CURSOR":"s=0123456789abcdef0123456789abcdef;i=%x;b=fedcba9876543210fedcba9876543210;m=1;t=1;x=1","__REALTIME_TIMESTAMP":"%d","PRIORITY":"6","_BOOT_ID":"x","MESSAGE":%s}`,
		n, 1792843200000000+int64(n)*1000000, message) + "\n"
}

func cursorOf(n int) string {
	return fmt.Sprintf("s=0123456789abcdef0123456789abcdef;i=%x;b=fedcba9876543210fedcba9876543210;m=1;t=1;x=1", n)
}

func logCommand(after string, limit int) Command {
	return Command{OperationID: testOpID, ServerID: testServerID, Action: helperv1.Action_ACTION_READ_LOG,
		Log: LogCommand{AfterCursor: after, Limit: limit}}
}

func texts(r *helperv1.LogResult) []string {
	var out []string
	for _, l := range r.GetLines() {
		out = append(out, l.GetText())
	}
	return out
}

func TestJournalArgv(t *testing.T) {
	base := []string{"/usr/bin/journalctl", "--no-pager", "--quiet", "--output=json", "--output-fields=MESSAGE,PRIORITY", "--unit=" + testUnit}
	runner := &fakeCappedRunner{}
	j := Journal{Runner: runner}
	if _, err := j.Read(context.Background(), logCommand("", 50)); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Read(context.Background(), logCommand(testCursor, 50)); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		append(slices.Clone(base), "--reverse", "--lines=50"),
		append(slices.Clone(base), "--after-cursor="+testCursor),
	}
	if !slices.EqualFunc(runner.calls, want, slices.Equal) {
		t.Errorf("argv\n%q\nwant\n%q", runner.calls, want)
	}
	if !slices.Equal(runner.maxes, []int{journalReadBytes, journalReadBytes}) {
		t.Errorf("max %v, want %d each", runner.maxes, journalReadBytes)
	}
}

// Without a cursor journalctl gives the newest first; the answer is the
// same lines oldest first, with the newest one's cursor.
func TestJournalTail(t *testing.T) {
	out := entry(3, `"three"`) + entry(2, `"two"`) + entry(1, `"one"`)
	r, err := Journal{Runner: &fakeCappedRunner{output: out}}.Read(context.Background(), logCommand("", 3))
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(r); !slices.Equal(got, []string{"one", "two", "three"}) {
		t.Errorf("lines %q", got)
	}
	if r.GetCursor() != cursorOf(3) || r.GetMore() {
		t.Errorf("cursor %q more %t, want the newest's and false", r.GetCursor(), r.GetMore())
	}
	first := r.GetLines()[0]
	if first.GetTimeUnixMicro() != 1792843201000000 || first.GetPriority() != 6 {
		t.Errorf("first line %v", first)
	}
}

func TestJournalEmpty(t *testing.T) {
	for _, after := range []string{"", testCursor} {
		r, err := Journal{Runner: &fakeCappedRunner{}}.Read(context.Background(), logCommand(after, 10))
		if err != nil {
			t.Fatal(err)
		}
		if len(r.GetLines()) != 0 || r.GetCursor() != "" || r.GetMore() {
			t.Errorf("after %q: %v, want nothing", after, r)
		}
	}
}

// After a cursor: oldest first, at most limit, and more when the rest
// didn't fit.
func TestJournalAfterCursor(t *testing.T) {
	out := entry(1, `"one"`) + entry(2, `"two"`) + entry(3, `"three"`)
	j := Journal{Runner: &fakeCappedRunner{output: out}}
	r, err := j.Read(context.Background(), logCommand(testCursor, 2))
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(r); !slices.Equal(got, []string{"one", "two"}) || r.GetCursor() != cursorOf(2) || !r.GetMore() {
		t.Errorf("limit 2: %q %q %t", got, r.GetCursor(), r.GetMore())
	}
	r, err = j.Read(context.Background(), logCommand(testCursor, 3))
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(r); len(got) != 3 || r.GetCursor() != cursorOf(3) || r.GetMore() {
		t.Errorf("limit 3: %q %q %t", got, r.GetCursor(), r.GetMore())
	}
}

// The text of one answer stays under MaxTextBytes: after a cursor the
// rest is more; without one the oldest are left out.
func TestJournalTextBudget(t *testing.T) {
	big := `"` + strings.Repeat("a", serverlog.MaxLineBytes) + `"`
	n := serverlog.MaxTextBytes/serverlog.MaxLineBytes + 3
	var forward, reverse string
	for i := 1; i <= n; i++ {
		forward += entry(i, big)
		reverse = entry(i, big) + reverse
	}
	fit := serverlog.MaxTextBytes / serverlog.MaxLineBytes

	r, err := Journal{Runner: &fakeCappedRunner{output: forward}}.Read(context.Background(), logCommand(testCursor, 200))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.GetLines()) != fit || !r.GetMore() || r.GetCursor() != cursorOf(fit) {
		t.Errorf("after a cursor: %d lines, more %t, cursor %q; want %d, true, the last kept", len(r.GetLines()), r.GetMore(), r.GetCursor(), fit)
	}

	r, err = Journal{Runner: &fakeCappedRunner{output: reverse}}.Read(context.Background(), logCommand("", 200))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.GetLines()) != fit || r.GetMore() || r.GetCursor() != cursorOf(n) {
		t.Errorf("newest: %d lines, more %t, cursor %q; want %d, false, the newest", len(r.GetLines()), r.GetMore(), r.GetCursor(), fit)
	}
	if got := r.GetLines()[0].GetTimeUnixMicro(); got != 1792843200000000+int64(n-fit+1)*1000000 {
		t.Errorf("oldest kept line at %d, want entry %d", got, n-fit+1)
	}
	total := 0
	for _, l := range r.GetLines() {
		total += len(l.GetText())
	}
	if total > serverlog.MaxTextBytes {
		t.Errorf("%d bytes of text", total)
	}
}

// A capped read drops the half entry at the end and says more.
func TestJournalCapped(t *testing.T) {
	out := entry(1, `"one"`) + entry(2, `"two"`)
	runner := &capAt{out: out, at: len(entry(1, `"one"`)) + 10}
	r, err := Journal{Runner: runner}.Read(context.Background(), logCommand(testCursor, 10))
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(r); !slices.Equal(got, []string{"one"}) || !r.GetMore() || r.GetCursor() != cursorOf(1) {
		t.Errorf("%q more %t cursor %q", got, r.GetMore(), r.GetCursor())
	}
	// Not even one whole entry: there's no way forward, so it's an error.
	runner.at = 10
	if _, err := (Journal{Runner: runner}).Read(context.Background(), logCommand(testCursor, 10)); !errors.Is(err, ErrJournal) {
		t.Errorf("no whole entry: %v, want ErrJournal", err)
	}
}

// capAt caps the output at a fixed length.
type capAt struct {
	out string
	at  int
}

func (c *capAt) RunCapped(_ context.Context, _ []string, max int) ([]byte, bool, error) {
	return []byte(c.out[:c.at]), true, nil
}

func TestJournalMessages(t *testing.T) {
	cases := map[string]string{
		`"plain"`:                             "plain",
		`[104,105]`:                           "hi",
		`[104,255]`:                           "h\uFFFD",
		`null`:                                tooLong,
		`["a","b"]`:                           "",
		`[1,300]`:                             "",
		`"\u001b[32mgreen\u001b[0m"`:          "\uFFFD[32mgreen\uFFFD[0m",
		`"tab\tok"`:                           "tab\tok",
		`"rtl \u202eevil"`:                    "rtl \uFFFDevil",
		`"cr\r\nlf"`:                          "cr\uFFFD\uFFFDlf",
		`"` + strings.Repeat("é", 3000) + `"`: strings.Repeat("é", serverlog.MaxLineBytes/2),
	}
	for raw, want := range cases {
		r, err := Journal{Runner: &fakeCappedRunner{output: entry(1, raw)}}.Read(context.Background(), logCommand("", 1))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got := texts(r); len(got) != 1 || got[0] != want {
			t.Errorf("MESSAGE %.40s: %q, want %.40q", raw, got, want)
		}
	}
}

func TestJournalPriority(t *testing.T) {
	cases := map[string]uint32{`"3"`: 3, `"0"`: 0, `"7"`: 7, `"8"`: 6, `"12"`: 6, `3`: 6, `null`: 6}
	for raw, want := range cases {
		line := strings.Replace(entry(1, `"x"`), `"PRIORITY":"6"`, `"PRIORITY":`+raw, 1)
		r, err := Journal{Runner: &fakeCappedRunner{output: line}}.Read(context.Background(), logCommand("", 1))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got := r.GetLines()[0].GetPriority(); got != want {
			t.Errorf("PRIORITY %s: %d, want %d", raw, got, want)
		}
	}
}

func TestJournalRefusesOddOutput(t *testing.T) {
	good := entry(1, `"x"`)
	cases := map[string]string{
		"not json":      "nope\n",
		"bad cursor":    strings.Replace(good, cursorOf(1), "s=1;evil=--x", 1),
		"no cursor":     strings.Replace(good, `"__CURSOR"`, `"OTHER"`, 1),
		"bad time":      strings.Replace(good, `"__REALTIME_TIMESTAMP":"1792843201000000"`, `"__REALTIME_TIMESTAMP":"soon"`, 1),
		"negative time": strings.Replace(good, `"__REALTIME_TIMESTAMP":"1792843201000000"`, `"__REALTIME_TIMESTAMP":"-5"`, 1),
		"no time":       strings.Replace(good, `"__REALTIME_TIMESTAMP"`, `"OTHER"`, 1),
		"empty line":    good + "\n",
	}
	for name, out := range cases {
		if _, err := (Journal{Runner: &fakeCappedRunner{output: out}}).Read(context.Background(), logCommand("", 5)); !errors.Is(err, ErrJournal) {
			t.Errorf("%s: %v, want ErrJournal", name, err)
		}
	}
	if _, err := (Journal{Runner: &fakeCappedRunner{err: errors.New("boom")}}).Read(context.Background(), logCommand("", 5)); err == nil {
		t.Error("a failed journalctl: no error")
	}
}

func TestValidateReadLog(t *testing.T) {
	req := func(after string, limit uint32) *helperv1.Request {
		r := validRequest(helperv1.Action_ACTION_READ_LOG)
		r.Log = &helperv1.LogSpec{AfterCursor: after, Limit: limit}
		return r
	}
	for _, c := range []struct {
		after string
		limit uint32
	}{{"", 1}, {"", 200}, {testCursor, 50}} {
		cmd, err := ValidateRequest(req(c.after, c.limit))
		if err != nil {
			t.Errorf("%q %d: %v", c.after, c.limit, err)
			continue
		}
		want := Command{OperationID: testOpID, ServerID: testServerID, Action: helperv1.Action_ACTION_READ_LOG,
			Log: LogCommand{AfterCursor: c.after, Limit: int(c.limit)}}
		if cmd != want {
			t.Errorf("got %+v, want %+v", cmd, want)
		}
	}

	withProvision := req("", 5)
	withProvision.Provision = &helperv1.ProvisionSpec{Runtime: "java25", HeapMib: 1024}
	withFile := req("", 5)
	withFile.File = &helperv1.FileSpec{Name: "eula.txt"}
	withDeadline := req("", 5)
	withDeadline.NotAfterUnixMs = testNotAfter
	statusWithLog := validRequest(helperv1.Action_ACTION_STATUS)
	statusWithLog.Log = &helperv1.LogSpec{Limit: 5}
	startWithLog := validRequest(helperv1.Action_ACTION_START)
	startWithLog.Log = &helperv1.LogSpec{Limit: 5}
	cases := map[string]*helperv1.Request{
		"no spec":           validRequest(helperv1.Action_ACTION_READ_LOG),
		"limit 0":           req("", 0),
		"limit 201":         req("", 201),
		"option cursor":     req("--since=yesterday", 5),
		"file cursor":       req("--file=/etc/shadow", 5),
		"cursor newline":    req(testCursor+"\n", 5),
		"limit 10000":       req("", 10000),
		"uppercase cursor":  req("s=ABC", 5),
		"long cursor":       req("s="+strings.Repeat("a", 300), 5),
		"provision spec":    withProvision,
		"file spec":         withFile,
		"deadline":          withDeadline,
		"STATUS with a log": statusWithLog,
		"START with a log":  startWithLog,
	}
	for name, r := range cases {
		if _, err := ValidateRequest(r); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v, want ErrInvalidRequest", name, err)
		}
	}
}

func TestRouterReadsLogs(t *testing.T) {
	runner := &fakeCappedRunner{output: entry(1, `"hello"`)}
	r := Router{Journal: &Journal{Runner: runner}}
	resp := r.Handle(context.Background(), logCommand("", 10))
	if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_UNSPECIFIED || len(resp.GetLog().GetLines()) != 1 {
		t.Fatalf("READ_LOG: %v %q %v", resp.GetErrorCode(), resp.GetErrorMessage(), resp.GetLog())
	}
	runner.err = errors.New("journalctl died")
	if resp := r.Handle(context.Background(), logCommand("", 10)); resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_FAILED || resp.GetLog() != nil {
		t.Errorf("failed read: %v %v, want FAILED and no lines", resp.GetErrorCode(), resp.GetLog())
	}

	// The provisioning helper never reads logs.
	f := newFixture(t)
	provisionOnly := Router{Provisioner: &f.p, Worker: &Worker{}}
	if resp := provisionOnly.Handle(context.Background(), logCommand("", 10)); resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST {
		t.Errorf("provisioner on READ_LOG: %v", resp.GetErrorCode())
	}
}

func TestExecRunnerRunCapped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, capped, err := ExecRunner{}.RunCapped(ctx, []string{"/bin/echo", "hi"}, 100)
	if err != nil || capped || string(out) != "hi\n" {
		t.Errorf("echo: %q %t %v", out, capped, err)
	}
	// yes never stops on its own: the cap has to end it.
	// Exactly max bytes isn't over it.
	out, capped, err = ExecRunner{}.RunCapped(ctx, []string{"/bin/echo", "hi"}, 3)
	if err != nil || capped || string(out) != "hi\n" {
		t.Errorf("exactly max: %q %t %v", out, capped, err)
	}
	out, capped, err = ExecRunner{}.RunCapped(ctx, []string{"/usr/bin/yes"}, 1000)
	if err != nil || !capped || len(out) != 1000 {
		t.Errorf("yes: %d bytes %t %v", len(out), capped, err)
	}
	if _, _, err := (ExecRunner{}).RunCapped(ctx, []string{"/bin/sh", "-c", "echo why >&2; exit 3"}, 100); err == nil || !strings.Contains(err.Error(), "why") {
		t.Errorf("failure: %v, want stderr in the error", err)
	}
	if _, _, err := (ExecRunner{}).RunCapped(ctx, nil, 100); err == nil {
		t.Error("empty argv: no error")
	}
}

// Audit 7M2: Read checks the command again itself, so nothing reaches
// journalctl that ValidateRequest would have refused.
func TestJournalRefusesBeforeExec(t *testing.T) {
	cases := map[string]Command{
		"option cursor":    logCommand("--file=/etc/shadow", 10),
		"cursor newline":   logCommand(testCursor+"\n", 10),
		"oversized cursor": logCommand("s="+strings.Repeat("a", 300), 10),
		"limit 0":          logCommand("", 0),
		"limit 10000":      logCommand("", 10000),
		"bad server":       {OperationID: testOpID, ServerID: "../../etc", Action: helperv1.Action_ACTION_READ_LOG, Log: LogCommand{Limit: 10}},
	}
	for name, cmd := range cases {
		runner := &fakeCappedRunner{}
		_, err := Journal{Runner: runner}.Read(context.Background(), cmd)
		if !errors.Is(err, ErrInvalidRequest) || len(runner.calls) != 0 {
			t.Errorf("%s: %v after %d runs, want ErrInvalidRequest before any", name, err, len(runner.calls))
		}
		resp := Router{Journal: &Journal{Runner: runner}}.Handle(context.Background(), cmd)
		if resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_INVALID_REQUEST {
			t.Errorf("%s through the router: %v", name, resp.GetErrorCode())
		}
	}
}

// journalctl gets 5 s, inside the connection's own deadline.
func TestJournalTimeout(t *testing.T) {
	var left time.Duration
	runner := &deadlineRunner{seen: &left}
	if _, err := (Journal{Runner: runner}).Read(context.Background(), logCommand("", 10)); err != nil {
		t.Fatal(err)
	}
	if left <= 4*time.Second || left > journalTimeout {
		t.Errorf("deadline %s away, want just under %s", left, journalTimeout)
	}
}

type deadlineRunner struct{ seen *time.Duration }

func (d *deadlineRunner) RunCapped(ctx context.Context, _ []string, _ int) ([]byte, bool, error) {
	dl, ok := ctx.Deadline()
	if ok {
		*d.seen = time.Until(dl)
	}
	return nil, false, nil
}

// Audit H1: at most MaxLogReads at once; one more is refused without
// running anything.
func TestJournalReadLimit(t *testing.T) {
	if cap(logReads) != MaxLogReads || MaxLogReads != 4 {
		t.Errorf("the process limit is %d (const %d), want 4", cap(logReads), MaxLogReads)
	}
	full := make(chan struct{}, 1)
	full <- struct{}{}
	runner := &fakeCappedRunner{}
	j := &Journal{Runner: runner, Reads: full}
	if _, err := j.Read(context.Background(), logCommand("", 10)); !errors.Is(err, ErrTooManyReads) || len(runner.calls) != 0 {
		t.Errorf("full: %v after %d runs", err, len(runner.calls))
	}
	if resp := (Router{Journal: j}).Handle(context.Background(), logCommand("", 10)); resp.GetErrorCode() != helperv1.ErrorCode_ERROR_CODE_CONFLICT {
		t.Errorf("full through the router: %v", resp.GetErrorCode())
	}
	// A finished read gives its place back.
	<-full
	if _, err := j.Read(context.Background(), logCommand("", 10)); err != nil || len(full) != 0 {
		t.Errorf("after one freed: %v, %d held", err, len(full))
	}
}
