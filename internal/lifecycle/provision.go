package lifecycle

import (
	"context"
	"errors"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// ProvisionSpec says what a new server needs from the machine.
type ProvisionSpec struct {
	// Runtime is a name the helper knows, such as "java25". Never a path.
	Runtime string
	// HeapMiB is the Java heap in MiB.
	HeapMiB uint32
}

// ErrConflict is returned for the helper's ERROR_CODE_CONFLICT: something
// the server needs already exists and Waystone did not create it.
var ErrConflict = errors.New("helper refused: conflict")

// Provision implements Runtime. It sends ACTION_PROVISION with the spec
// through h.Provisioner (not h.Client) and returns the response's changed
// flag.
//
// Use the same error mapping as Start, Stop and Status: see the README for
// splitting do into do + send. Never duplicate the mapping here.
func (h HelperRuntime) Provision(ctx context.Context, serverID, operationID string, spec ProvisionSpec) (bool, error) {
	req := &helperv1.Request{
		OperationId: operationID,
		ServerId:    serverID,
		Action:      helperv1.Action_ACTION_PROVISION,
		Provision: &helperv1.ProvisionSpec{
			Runtime: spec.Runtime,
			HeapMib: spec.HeapMiB,
		},
	}

	resp, err := h.send(ctx, h.Provisioner, req)
	if err != nil {
		return false, err
	}

	return resp.GetChanged(), nil
}
