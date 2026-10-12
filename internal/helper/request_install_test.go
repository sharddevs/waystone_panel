// SPDX-License-Identifier: AGPL-3.0-only

package helper

import (
	"crypto/sha256"
	"errors"
	"testing"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"google.golang.org/protobuf/encoding/protowire"
)

// jarHash is the SHA-256 of "jar", used by all the install tests.
var jarHash = sha256.Sum256([]byte("jar"))

func installSpec() *helperv1.FileSpec {
	return &helperv1.FileSpec{Name: "server.jar", ContentSha256: jarHash[:]}
}

func TestValidateFileSpecInstall(t *testing.T) {
	got, err := ValidateFileSpec(helperv1.Action_ACTION_INSTALL_FILE, installSpec())
	if want := (FileCommand{Name: "server.jar", ContentSHA256: jarHash}); err != nil || got != want {
		t.Errorf("install: %+v, %v; want %+v", got, err, want)
	}
}

func TestValidateFileSpecInstallRejects(t *testing.T) {
	// Field 99 isn't part of FileSpec, so proto keeps it as an unknown field.
	unknownField := installSpec()
	unknownField.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
	tests := map[string]*helperv1.FileSpec{
		"missing spec":   nil,
		"unknown field":  unknownField,
		"bad name":       {Name: "../server.jar", ContentSha256: jarHash[:]},
		"hidden name":    {Name: ".server.jar", ContentSha256: jarHash[:]},
		"no hash":        {Name: "server.jar"},
		"short hash":     {Name: "server.jar", ContentSha256: jarHash[:31]},
		"long hash":      {Name: "server.jar", ContentSha256: append(jarHash[:], 0)},
		"with content":   {Name: "server.jar", ContentSha256: jarHash[:], Content: []byte("jar")},
		"expect absent":  {Name: "server.jar", ContentSha256: jarHash[:], ExpectAbsent: true},
		"expect content": {Name: "server.jar", ContentSha256: jarHash[:], ExpectSha256: jarHash[:]},
	}
	for name, spec := range tests {
		if _, err := ValidateFileSpec(helperv1.Action_ACTION_INSTALL_FILE, spec); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v, want ErrInvalidRequest", name, err)
		}
	}
}

// Only installs use content_sha256. A read or write that carries one is
// confused or forged.
func TestValidateFileSpecContentHashOnlyForInstall(t *testing.T) {
	tests := map[helperv1.Action]*helperv1.FileSpec{
		helperv1.Action_ACTION_READ_FILE:  {Name: "f.txt", ContentSha256: jarHash[:]},
		helperv1.Action_ACTION_WRITE_FILE: {Name: "f.txt", Content: []byte("x"), ExpectAbsent: true, ContentSha256: jarHash[:]},
	}
	for action, spec := range tests {
		if _, err := ValidateFileSpec(action, spec); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%v with content_sha256: %v, want ErrInvalidRequest", action, err)
		}
	}
}

func TestValidateRequestInstall(t *testing.T) {
	req := validRequest(helperv1.Action_ACTION_INSTALL_FILE)
	req.File = installSpec()
	cmd, err := ValidateRequest(req)
	want := Command{
		OperationID: testOpID, ServerID: testServerID, Action: helperv1.Action_ACTION_INSTALL_FILE,
		File: FileCommand{Name: "server.jar", ContentSHA256: jarHash},
	}
	// Command is comparable, so != checks every field. Input stays nil
	// because only the Server attaches the descriptor.
	if err != nil || cmd != want {
		t.Errorf("ValidateRequest = %+v, %v; want %+v", cmd, err, want)
	}
}

func TestValidateRequestInstallRejects(t *testing.T) {
	noFile := validRequest(helperv1.Action_ACTION_INSTALL_FILE)
	withProvision := validRequest(helperv1.Action_ACTION_INSTALL_FILE)
	withProvision.File = installSpec()
	withProvision.Provision = validSpec()
	startWithFile := validRequest(helperv1.Action_ACTION_START)
	startWithFile.File = installSpec()
	for name, req := range map[string]*helperv1.Request{
		"no file spec":      noFile,
		"provision spec":    withProvision,
		"file spec on stop": startWithFile,
	} {
		if _, err := ValidateRequest(req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v, want ErrInvalidRequest", name, err)
		}
	}
}
