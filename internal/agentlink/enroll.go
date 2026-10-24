// SPDX-License-Identifier: AGPL-3.0-only

package agentlink

import (
	"context"
	"crypto/sha256"
	"net"
	"net/netip"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/store"
)

// Enroll's limits (D2, 6H7).
const (
	// enrollPerIP attempts per address in enrollWindow.
	enrollPerIP  = 10
	enrollWindow = 15 * time.Minute
	// There is no cap across addresses that skips the database: it would
	// refuse a real token during a flood (6a-H1). Unknown secrets write
	// nothing, and enroll_node audits at most 5 refusals per token.
	// enrollTimeout bounds the database call.
	enrollTimeout = 5 * time.Second
)

// MsgEnrollmentRefused is Enroll's one answer for every kind of failure:
// unknown, used or expired token, key already a node's, too many tries.
const MsgEnrollmentRefused = "enrollment_refused"

var errEnrollRefused = streamError(codes.PermissionDenied, MsgEnrollmentRefused)

// enrollServer is the EnrollService. Only the TCP gRPC server has it.
type enrollServer struct {
	agentv1.UnimplementedEnrollServiceServer
	s *Server
}

// Enroll redeems a token for the key this connection's handshake proved.
// The key never comes from the request (6M1).
func (e *enrollServer) Enroll(ctx context.Context, req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
	s := e.s
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, errNotPermitted
	}
	auth, ok := p.AuthInfo.(TLSAuth)
	if !ok {
		return nil, errNotPermitted
	}
	ip := addrIP(p.Addr)
	if auth.conn != nil {
		// One call per connection (6a-H2).
		if !auth.conn.claimEnroll() {
			s.enrollLog("second_call", ip, "")
			return nil, errEnrollRefused
		}
		defer auth.conn.enrollDone()
	}
	if !s.enrollPerIP.allow(addrKey(ip), s.now()) {
		s.enrollLog("too_many", ip, "")
		return nil, errEnrollRefused
	}
	if hasUnknownFields(req.ProtoReflect()) || len(req.GetSecret()) != 32 {
		s.enrollLog("bad_request", ip, "")
		return nil, errEnrollRefused
	}
	requestID, err := ids.NewV7()
	if err != nil {
		return nil, streamError(codes.Unavailable, "enroll_failed")
	}
	clientIP := ""
	if ip.IsValid() {
		clientIP = ip.String()
	}
	dbctx, cancel := context.WithTimeout(ctx, enrollTimeout)
	defer cancel()
	r, err := s.store.EnrollNode(dbctx, sha256.Sum256(req.GetSecret()), auth.Pin, store.EnrollAudit{RequestID: requestID, ClientIP: clientIP})
	if err != nil {
		s.logf("agent_enroll=failed request=%s ip=%s error=%q", requestID, clientIP, err.Error())
		return nil, errEnrollRefused
	}
	if r.Outcome != store.EnrollEnrolled {
		s.enrollLog(string(r.Outcome), ip, requestID)
		return nil, errEnrollRefused
	}
	s.logf("agent_enroll=enrolled request=%s ip=%s node=%s pin=%s", requestID, clientIP, r.NodeID, auth.Pin)
	return &agentv1.EnrollResponse{NodeId: r.NodeID, Name: r.Name}, nil
}

// enrollLog writes a refusal's line, at most one a minute per reason and
// address (by addrKey), with how many like it were left out since the
// last (6a Q-b). The secret never appears.
func (s *Server) enrollLog(reason string, ip netip.Addr, requestID string) {
	key := addrKey(ip)
	if ok, skipped := s.logLimit.allowCount("enroll/" + reason + "/" + key); ok {
		s.logf("agent_enroll=refused reason=%s addr=%s ip=%s request=%s skipped=%d", reason, key, ip, requestID, skipped)
	}
}

func addrIP(a net.Addr) netip.Addr {
	if t, ok := a.(*net.TCPAddr); ok {
		if ip, ok := netip.AddrFromSlice(t.IP); ok {
			return ip.Unmap()
		}
	}
	return netip.Addr{}
}

// window allows at most n events per key in a sliding window, in memory.
type window struct {
	mu     sync.Mutex
	n      int
	length time.Duration
	events map[string][]time.Time
}

func newWindow(n int, length time.Duration) *window {
	return &window{n: n, length: length, events: map[string][]time.Time{}}
}

// allow counts an event for key at now if there is room, and reports
// whether there was. Keys with nothing left in the window are dropped,
// so the map stays as small as the traffic.
func (w *window) allow(key string, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, ts := range w.events {
		kept := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < w.length {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(w.events, k)
		} else {
			w.events[k] = kept
		}
	}
	if len(w.events[key]) >= w.n {
		return false
	}
	w.events[key] = append(w.events[key], now)
	return true
}
