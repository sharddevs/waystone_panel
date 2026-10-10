package helper

import (
	"errors"
	"testing"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
	"google.golang.org/protobuf/encoding/protowire"
)

const (
	testOpID     = "0192a5c4-7d3e-7b1a-9c2f-3e4d5f6a7b8c"
	testServerID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
)

func validRequest(action helperv1.Action) *helperv1.Request {
	return &helperv1.Request{OperationId: testOpID, ServerId: testServerID, Action: action}
}

func TestValidateRequestAccepts(t *testing.T) {
	for _, action := range []helperv1.Action{
		helperv1.Action_ACTION_START,
		helperv1.Action_ACTION_STOP,
		helperv1.Action_ACTION_STATUS,
	} {
		cmd, err := ValidateRequest(validRequest(action))
		if err != nil {
			t.Errorf("%v: unexpected error %v", action, err)
			continue
		}
		want := Command{OperationID: testOpID, ServerID: testServerID, Action: action}
		if cmd != want {
			t.Errorf("%v: got %+v, want %+v", action, cmd, want)
		}
	}
}

func TestValidateRequestRejects(t *testing.T) {
	unknownField := validRequest(helperv1.Action_ACTION_START)
	// Field 99 does not exist in Request. A newer agent might send it; an
	// older helper must refuse rather than silently ignore it.
	unknownField.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))

	cases := map[string]*helperv1.Request{
		"nil request":          nil,
		"empty request":        {},
		"unspecified action":   validRequest(helperv1.Action_ACTION_UNSPECIFIED),
		"undefined action 99":  validRequest(helperv1.Action(99)),
		"missing operation id": {ServerId: testServerID, Action: helperv1.Action_ACTION_START},
		"bad operation id":     {OperationId: "op-1", ServerId: testServerID, Action: helperv1.Action_ACTION_START},
		"missing server id":    {OperationId: testOpID, Action: helperv1.Action_ACTION_START},
		"uppercase server id":  {OperationId: testOpID, ServerId: "3F2504E0-4F89-41D3-9A0C-0305E82C3301", Action: helperv1.Action_ACTION_START},
		"path as server id":    {OperationId: testOpID, ServerId: "../../etc/passwd", Action: helperv1.Action_ACTION_START},
		"unit name as server":  {OperationId: testOpID, ServerId: "sshd.service", Action: helperv1.Action_ACTION_STOP},
		"unknown field":        unknownField,
	}
	for name, req := range cases {
		if _, err := ValidateRequest(req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: got %v, want ErrInvalidRequest", name, err)
		}
	}
}

func TestAuthorize(t *testing.T) {
	const agent = 998
	if err := Authorize(PeerCred{UID: agent, PID: 42}, agent); err != nil {
		t.Errorf("agent uid refused: %v", err)
	}
	for name, peer := range map[string]PeerCred{
		"other user":         {UID: 1000},
		"root":               {UID: 0},
		"agent's group only": {UID: 1000, GID: agent},
	} {
		if err := Authorize(peer, agent); !errors.Is(err, ErrPermissionDenied) {
			t.Errorf("%s: got %v, want ErrPermissionDenied", name, err)
		}
	}
	// A helper configured with uid 0 as the agent is misconfigured and must
	// allow nobody, not everyone running as root.
	if err := Authorize(PeerCred{UID: 0}, 0); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("agentUID 0: got %v, want ErrPermissionDenied", err)
	}
}

func TestUnitName(t *testing.T) {
	got := UnitName(testServerID)
	want := "waystone-srv-3f2504e0-4f89-41d3-9a0c-0305e82c3301.service"
	if got != want {
		t.Errorf("UnitName = %q, want %q", got, want)
	}
}
