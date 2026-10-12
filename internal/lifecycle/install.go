// SPDX-License-Identifier: AGPL-3.0-only

package lifecycle

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"

	helperv1 "github.com/sharddevs/waystone_panel/gen/waystone/helper/v1"
)

// InstallFile implements Runtime. It passes content's descriptor to the
// provisioner, which has the worker install it as the server's user once the
// SHA-256 matches sum. content still belongs to the caller afterwards.
func (h HelperRuntime) InstallFile(
	ctx context.Context,
	serverID, operationID, name string,
	content *os.File,
	sum [sha256.Size]byte,
) error {
	if content == nil {
		return fmt.Errorf("%w: missing installation content", ErrInvalidRequest)
	}

	req := &helperv1.Request{
		OperationId: operationID,
		ServerId:    serverID,
		Action:      helperv1.Action_ACTION_INSTALL_FILE,
		File: &helperv1.FileSpec{
			Name:          name,
			ContentSha256: sum[:],
		},
	}

	_, err := h.sendInput(ctx, h.Provisioner, req, content)
	return err
}
