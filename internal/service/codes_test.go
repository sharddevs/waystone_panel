// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sharddevs/waystone_panel/internal/serverlog"
	"github.com/sharddevs/waystone_panel/internal/store"
)

// V5: the API's own 503 is controller_busy; server_busy is left to the
// operation error code, and the two never share a string again.
func TestControllerBusyCode(t *testing.T) {
	if CodeControllerBusy != "controller_busy" || errorInfo[CodeControllerBusy].status != http.StatusServiceUnavailable {
		t.Errorf("controller_busy = %q %d", CodeControllerBusy, errorInfo[CodeControllerBusy].status)
	}
	if _, ok := errorInfo["server_busy"]; ok {
		t.Error("server_busy is still an API error code")
	}
	if CodeNodeUnreachable != "node_unreachable" || errorInfo[CodeNodeUnreachable].status != http.StatusConflict {
		t.Errorf("node_unreachable = %q %d", CodeNodeUnreachable, errorInfo[CodeNodeUnreachable].status)
	}
}

func TestCheckQuery(t *testing.T) {
	cases := []struct {
		query   string
		allowed []string
		ok      bool
	}{
		{"", nil, true},
		{"", []string{"limit"}, true},
		{"limit=5", []string{"limit"}, true},
		{"limit=5&server_id=x", []string{"server_id", "limit"}, true},
		{"x=1", nil, false},
		{"verbose", nil, false},
		{"limit=5&x=", []string{"limit"}, false},
		{"serverid=x", []string{"server_id", "limit"}, false},
		{"limit=%zz", []string{"limit"}, false},
		{"LIMIT=5", []string{"limit"}, false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/x?"+tc.query, nil)
		err := checkQuery(r, tc.allowed...)
		if (err == nil) != tc.ok {
			t.Errorf("%q with %v: err = %v, want ok=%v", tc.query, tc.allowed, err, tc.ok)
		}
		if err != nil && err.code != CodeBadRequest {
			t.Errorf("%q: code %s, want bad_request", tc.query, err.code)
		}
	}
}

// permits follows authorize, so the actions list leaves out exactly what
// the POST would refuse with forbidden (B8).
func TestPermits(t *testing.T) {
	for _, a := range []store.Action{store.ActionStart, store.ActionStop, store.ActionRestart} {
		for _, role := range []string{"admin", "node", "viewer", ""} {
			if got, want := permits(role)(a), authorize(role, string(a)); got != want {
				t.Errorf("permits(%q)(%s) = %v, authorize says %v", role, a, got, want)
			}
		}
		if permits("node")(a) {
			t.Errorf("role node may %s", a)
		}
	}
}

// 7a: each way a log read fails has its code.
func TestLogError(t *testing.T) {
	cases := map[error]string{
		serverlog.ErrUnreachable:               CodeNodeUnreachable,
		fmt.Errorf("x: %w", serverlog.ErrBusy): CodeControllerBusy,
		serverlog.ErrUnavailable:               CodeLogsUnavailable,
		errors.New("anything else"):            CodeInternal,
	}
	for err, want := range cases {
		if got := logError(err); got.code != want || !errors.Is(got, err) {
			t.Errorf("%v: %s, want %s with the cause kept", err, got.code, want)
		}
	}
	if CodeLogsUnavailable != "logs_unavailable" || errorInfo[CodeLogsUnavailable].status != http.StatusServiceUnavailable {
		t.Errorf("logs_unavailable = %q %d", CodeLogsUnavailable, errorInfo[CodeLogsUnavailable].status)
	}
}
