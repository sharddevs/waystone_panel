// SPDX-License-Identifier: AGPL-3.0-only

//go:build !devweb

package web

// devHTTP is false in release builds: the public URL must be https (W7).
const devHTTP = false
