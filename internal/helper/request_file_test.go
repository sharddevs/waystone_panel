// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"github.com/sharddevs/waystone_panel/internal/worker"
	"google.golang.org/protobuf/encoding/protowire"
)

func oldHash() []byte {
	h := sha256.Sum256([]byte("old"))
	return h[:]
}

func TestValidateFileSpecAccepts(t *testing.T) {
	got, err := ValidateFileSpec(helperv1.Action_ACTION_READ_FILE, &helperv1.FileSpec{Name: "server.properties"})
	if err != nil || got != (FileCommand{Name: "server.properties"}) {
		t.Errorf("read: %+v, %v", got, err)
	}

	got, err = ValidateFileSpec(helperv1.Action_ACTION_WRITE_FILE, &helperv1.FileSpec{Name: "eula.txt", Content: []byte("eula=true\n"), ExpectAbsent: true})
	if want := (FileCommand{Name: "eula.txt", Content: "eula=true\n", Expect: worker.ExpectAbsent()}); err != nil || got != want {
		t.Errorf("write absent: %+v, %v; want %+v", got, err, want)
	}

	got, err = ValidateFileSpec(helperv1.Action_ACTION_WRITE_FILE, &helperv1.FileSpec{Name: "f.txt", Content: []byte("new"), ExpectSha256: oldHash()})
	if want := (FileCommand{Name: "f.txt", Content: "new", Expect: worker.ExpectContent([]byte("old"))}); err != nil || got != want {
		t.Errorf("write over: %+v, %v; want %+v", got, err, want)
	}

	// Writing an empty file and writing exactly the limit are both fine.
	for _, n := range []int{0, worker.MaxFileBytes} {
		spec := &helperv1.FileSpec{Name: "f.txt", Content: make([]byte, n), ExpectAbsent: true}
		if _, err := ValidateFileSpec(helperv1.Action_ACTION_WRITE_FILE, spec); err != nil {
			t.Errorf("%d bytes: %v", n, err)
		}
	}
}

func TestValidateFileSpecRejects(t *testing.T) {
	unknownField := &helperv1.FileSpec{Name: "f.txt"}
	unknownField.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
	read, write := helperv1.Action_ACTION_READ_FILE, helperv1.Action_ACTION_WRITE_FILE

	tests := map[string]struct {
		action helperv1.Action
		spec   *helperv1.FileSpec
	}{
		"nil spec":                 {read, nil},
		"unknown field":            {read, unknownField},
		"path":                     {read, &helperv1.FileSpec{Name: "../../etc/shadow"}},
		"subdirectory":             {read, &helperv1.FileSpec{Name: "world/level.dat"}},
		"hidden":                   {read, &helperv1.FileSpec{Name: ".bashrc"}},
		"read with content":        {read, &helperv1.FileSpec{Name: "f.txt", Content: []byte("x")}},
		"read with absent":         {read, &helperv1.FileSpec{Name: "f.txt", ExpectAbsent: true}},
		"read with hash":           {read, &helperv1.FileSpec{Name: "f.txt", ExpectSha256: oldHash()}},
		"write, no expectation":    {write, &helperv1.FileSpec{Name: "f.txt", Content: []byte("x")}},
		"write, both":              {write, &helperv1.FileSpec{Name: "f.txt", Content: []byte("x"), ExpectAbsent: true, ExpectSha256: oldHash()}},
		"write, short hash":        {write, &helperv1.FileSpec{Name: "f.txt", Content: []byte("x"), ExpectSha256: oldHash()[:31]}},
		"write, long hash":         {write, &helperv1.FileSpec{Name: "f.txt", Content: []byte("x"), ExpectSha256: append(oldHash(), 0)}},
		"write, too large":         {write, &helperv1.FileSpec{Name: "f.txt", Content: make([]byte, worker.MaxFileBytes+1), ExpectAbsent: true}},
		"write, bad name":          {write, &helperv1.FileSpec{Name: "a/b", Content: []byte("x"), ExpectAbsent: true}},
		"wrong action (start)":     {helperv1.Action_ACTION_START, &helperv1.FileSpec{Name: "f.txt"}},
		"wrong action (provision)": {helperv1.Action_ACTION_PROVISION, &helperv1.FileSpec{Name: "f.txt"}},
	}
	for name, tt := range tests {
		if _, err := ValidateFileSpec(tt.action, tt.spec); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
}

func TestValidateRequestFileActions(t *testing.T) {
	req := validRequest(helperv1.Action_ACTION_READ_FILE)
	req.File = &helperv1.FileSpec{Name: "server.properties"}
	cmd, err := ValidateRequest(req)
	want := Command{OperationID: testOpID, ServerID: testServerID, Action: helperv1.Action_ACTION_READ_FILE, File: FileCommand{Name: "server.properties"}}
	if err != nil || cmd != want {
		t.Errorf("READ_FILE: %+v, %v; want %+v", cmd, err, want)
	}

	req = validRequest(helperv1.Action_ACTION_WRITE_FILE)
	req.File = &helperv1.FileSpec{Name: "eula.txt", Content: []byte("eula=true\n"), ExpectAbsent: true}
	cmd, err = ValidateRequest(req)
	want = Command{
		OperationID: testOpID, ServerID: testServerID, Action: helperv1.Action_ACTION_WRITE_FILE,
		File: FileCommand{Name: "eula.txt", Content: "eula=true\n", Expect: worker.ExpectAbsent()},
	}
	if err != nil || cmd != want {
		t.Errorf("WRITE_FILE: %+v, %v; want %+v", cmd, err, want)
	}
}

// Each spec belongs to its own actions. Anything attached elsewhere is
// refused, never ignored.
func TestValidateRequestFileActionsRejects(t *testing.T) {
	file := &helperv1.FileSpec{Name: "f.txt"}
	with := func(action helperv1.Action, provision *helperv1.ProvisionSpec, f *helperv1.FileSpec) *helperv1.Request {
		req := validRequest(action)
		req.Provision = provision
		req.File = f
		return req
	}
	tests := map[string]*helperv1.Request{
		"read without spec":    with(helperv1.Action_ACTION_READ_FILE, nil, nil),
		"write without spec":   with(helperv1.Action_ACTION_WRITE_FILE, nil, nil),
		"read with provision":  with(helperv1.Action_ACTION_READ_FILE, validSpec(), file),
		"provision with file":  with(helperv1.Action_ACTION_PROVISION, validSpec(), file),
		"start with file":      with(helperv1.Action_ACTION_START, nil, file),
		"status with file":     with(helperv1.Action_ACTION_STATUS, nil, file),
		"read with a bad name": with(helperv1.Action_ACTION_READ_FILE, nil, &helperv1.FileSpec{Name: "/etc/shadow"}),
		"bad op ID for a read": {
			OperationId: "../x", ServerId: testServerID, Action: helperv1.Action_ACTION_READ_FILE, File: file,
		},
	}
	for name, req := range tests {
		if _, err := ValidateRequest(req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
}

// The refusal reaches the agent and the log once, not as
// "invalid request: invalid request: ...".
func TestValidateRequestFileMessage(t *testing.T) {
	req := validRequest(helperv1.Action_ACTION_READ_FILE)
	req.File = &helperv1.FileSpec{Name: "../x"}
	_, err := ValidateRequest(req)
	if err == nil {
		t.Fatal("../x accepted")
	}
	if n := strings.Count(err.Error(), "invalid request"); n != 1 {
		t.Errorf("%q says %q %d times, want once", err, "invalid request", n)
	}
}

// A whole file plus the request around it must fit in one frame.
func TestMaxFileFitsInAFrame(t *testing.T) {
	if worker.MaxFileBytes+4096 > MaxFrameSize {
		t.Errorf("MaxFileBytes %d leaves under 4 KiB of a %d-byte frame", worker.MaxFileBytes, MaxFrameSize)
	}
}
