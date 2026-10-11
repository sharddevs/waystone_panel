package lifecycle

import (
	"context"
	"errors"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// ProvisionSpec says what a new server needs from the machine.
type ProvisionSpec struct {
	// Runtime is a name the helper knows, such as "java25". The helper
	// refuses anything else, including paths.
	Runtime string
	// HeapMiB is the Java heap in MiB.
	HeapMiB uint32
}

// ErrConflict means something the server needs already exists and
// Waystone did not create it, so the helper won't take it over.
var ErrConflict = errors.New("helper refused: conflict")

// Provision implements Runtime. It goes to the provisioning service, not
// the runtime helper, and reports whether anything on the machine changed.
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
