// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/sharddevs/waystone_panel/internal/ids"
)

// Enrolling a node and revoking it, seen from the database (6a-1,
// migration 00008). The agent link (6a-2) proves the key; this file only
// stores tokens, redeems them and finds nodes by key.

// EnrollmentTTL is how long a token can be redeemed. The database's own
// default sets it; this only says what it is.
const EnrollmentTTL = time.Hour

// Enrollment is one token, without its secret or the secret's hash.
type Enrollment struct {
	ID        string
	Name      string
	CreatedBy Actor
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
	// NodeID is the node it became; empty if unused, or if the key was
	// already a node's.
	NodeID string
}

// MaxNodeNameBytes keeps "node <name>", the node's actor name, within
// an actor name's 100 bytes.
const MaxNodeNameBytes = 64

// ValidNodeName reports whether name can be a new node's name: 1 to
// MaxNodeNameBytes bytes of UTF-8 without control characters.
func ValidNodeName(name string) bool {
	if len(name) < 1 || len(name) > MaxNodeNameBytes || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (t *Tx) requireAdmin() error {
	if t.role != RoleAdmin {
		return ErrNotAdmin
	}
	return nil
}

// CreateEnrollment makes a one-time token for a node called name and
// returns it with its secret. Only the secret's SHA-256 is stored; the
// secret itself exists only in the return value. The actor must be an
// admin (ErrNotAdmin; the table's policy refuses anyone else too). An
// invalid name is ErrInvalidInput. When it expires and whether it has
// been used are the database's to say. A node.enrollment.create audit row
// labelled requestID is written with it (6b-H1): a token is the right to
// add a machine.
func (t *Tx) CreateEnrollment(ctx context.Context, name, requestID string) (Enrollment, [32]byte, error) {
	var secret [32]byte
	if err := t.requireAdmin(); err != nil {
		return Enrollment{}, secret, err
	}
	if !ValidNodeName(name) {
		return Enrollment{}, secret, ErrInvalidInput
	}
	id, err := ids.NewV7()
	if err != nil {
		return Enrollment{}, secret, fmt.Errorf("generate enrollment ID: %w", err)
	}
	if _, err := rand.Read(secret[:]); err != nil {
		return Enrollment{}, secret, fmt.Errorf("generate enrollment secret: %w", err)
	}
	hash := sha256.Sum256(secret[:])

	e := Enrollment{ID: id, Name: name, CreatedBy: t.actor}
	err = t.tx.QueryRow(ctx, `
		INSERT INTO waystone.node_enrollments
			(tenant_id, enrollment_id, name, secret_sha256, created_by_type, created_by_id, created_by_name)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING created_at, expires_at`,
		t.tenantID, id, name, hash[:], t.actor.Type, t.actor.ID, t.actor.Name).
		Scan(&e.CreatedAt, &e.ExpiresAt)
	if err != nil {
		return Enrollment{}, [32]byte{}, fmt.Errorf("create enrollment: %w", err)
	}
	if err := t.insertAudit(ctx, Denial{
		Action:       "node.enrollment.create",
		ResourceType: "node_enrollment",
		ResourceID:   id,
		Reason:       "created",
		RequestID:    requestID,
	}, "recorded"); err != nil {
		return Enrollment{}, [32]byte{}, err
	}
	return e, secret, nil
}

// ListEnrollments returns the tenant's newest tokens first, at most
// MaxListLimit. Admins only (ErrNotAdmin).
func (t *Tx) ListEnrollments(ctx context.Context) ([]Enrollment, error) {
	if err := t.requireAdmin(); err != nil {
		return nil, err
	}
	rows, err := t.tx.Query(ctx, `
		SELECT enrollment_id::text, name, created_by_type, created_by_id, created_by_name,
		       created_at, expires_at, used_at, COALESCE(node_id::text, '')
		FROM waystone.node_enrollments
		ORDER BY created_at DESC, enrollment_id DESC
		LIMIT $1`, MaxListLimit)
	if err != nil {
		return nil, fmt.Errorf("list enrollments: %w", err)
	}
	defer rows.Close()
	out := []Enrollment{}
	for rows.Next() {
		var e Enrollment
		if err := rows.Scan(&e.ID, &e.Name, &e.CreatedBy.Type, &e.CreatedBy.ID, &e.CreatedBy.Name,
			&e.CreatedAt, &e.ExpiresAt, &e.UsedAt, &e.NodeID); err != nil {
			return nil, fmt.Errorf("scan enrollment: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list enrollments: %w", err)
	}
	return out, nil
}

// EnrollOutcome is how a redemption ended.
type EnrollOutcome string

const (
	EnrollEnrolled EnrollOutcome = "enrolled"
	EnrollUsed     EnrollOutcome = "used"
	EnrollExpired  EnrollOutcome = "expired"
	EnrollPinTaken EnrollOutcome = "pin_taken"
	// EnrollUnknown: no such secret. Nothing is written, since there is
	// no tenant to file it under.
	EnrollUnknown EnrollOutcome = "unknown"
)

// Enrolled is a redemption's result. NodeID is set only for
// EnrollEnrolled; TenantID and Name for every outcome but EnrollUnknown.
type Enrolled struct {
	Outcome  EnrollOutcome
	NodeID   string
	TenantID string
	Name     string
}

// EnrollAudit labels the audit row of a redemption. Both are checked
// like a Denial's: RequestID 1 to 200 bytes without control characters,
// ClientIP empty or a canonical IP address.
type EnrollAudit struct {
	RequestID string
	ClientIP  string
}

// EnrollNode redeems the token whose secret hashes to secretSHA256 for
// the key pinned by spki, through waystone.enroll_node (6M3). There is
// no actor: the token is the authority, like a password at sign-in. spki
// must come from the TLS handshake, never from a request (6M1).
//
// Every outcome but EnrollUnknown leaves an audit row, except that a used
// or expired token is audited for its first 5 refusals only (6a-H1). Any
// error means nothing was redeemed.
func (s *Store) EnrollNode(ctx context.Context, secretSHA256, spki [32]byte, a EnrollAudit) (Enrolled, error) {
	if !validLabel(a.RequestID) {
		return Enrolled{}, ErrInvalidInput
	}
	var clientIP *string
	if a.ClientIP != "" {
		if !canonicalIP(a.ClientIP) {
			return Enrolled{}, ErrInvalidInput
		}
		clientIP = &a.ClientIP
	}
	var r Enrolled
	var outcome string
	var nodeID, tenantID *string
	err := s.pool.QueryRow(ctx, `
		SELECT outcome, node_id::text, tenant_id::text, name
		FROM waystone.enroll_node($1, $2, $3, $4)`,
		secretSHA256[:], spki[:], a.RequestID, clientIP).
		Scan(&outcome, &nodeID, &tenantID, &r.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Enrolled{Outcome: EnrollUnknown}, nil
	}
	if err != nil {
		return Enrolled{}, fmt.Errorf("enroll node: %w", err)
	}
	r.Outcome = EnrollOutcome(outcome)
	switch r.Outcome {
	case EnrollEnrolled, EnrollUsed, EnrollExpired, EnrollPinTaken:
	default:
		return Enrolled{}, fmt.Errorf("enroll node: unexpected outcome %q", outcome)
	}
	if nodeID != nil {
		r.NodeID = *nodeID
	}
	if tenantID != nil {
		r.TenantID = *tenantID
	}
	if (r.Outcome == EnrollEnrolled) != (r.NodeID != "") {
		return Enrolled{}, errors.New("enroll node: outcome and node disagree")
	}
	return r, nil
}

// AgentNode is a node as the agent link sees it before any tenant is
// known.
type AgentNode struct {
	NodeID   string
	TenantID string
	Name     string
	Revoked  bool
}

// Actor is the node's actor for its transactions, named "node <name>".
// A name too long for that (only an older node's can be) gives
// "node <ID>" instead.
func (n AgentNode) Actor() Actor {
	name := "node " + n.Name
	if len(name) > 100 {
		name = "node " + n.NodeID
	}
	return Actor{Type: ActorNode, ID: n.NodeID, Name: name}
}

// AgentNodeByPin finds the node whose key is pinned as spki (6M2), revoked
// or not. ErrNotFound if there is none.
func (s *Store) AgentNodeByPin(ctx context.Context, spki [32]byte) (AgentNode, error) {
	return s.agentNode(ctx, `SELECT node_id::text, tenant_id::text, name, revoked FROM waystone.agent_node_by_pin($1)`, spki[:])
}

// AgentNodeByID finds a node by its ID, for the local node on agent.sock
// (6H6). ErrNotFound if there is none, or the ID isn't a UUID.
func (s *Store) AgentNodeByID(ctx context.Context, nodeID string) (AgentNode, error) {
	if ids.Check(nodeID) != nil {
		return AgentNode{}, ErrNotFound
	}
	return s.agentNode(ctx, `SELECT node_id::text, tenant_id::text, name, revoked FROM waystone.agent_node_by_id($1)`, nodeID)
}

func (s *Store) agentNode(ctx context.Context, query string, arg any) (AgentNode, error) {
	var n AgentNode
	err := s.pool.QueryRow(ctx, query, arg).Scan(&n.NodeID, &n.TenantID, &n.Name, &n.Revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentNode{}, ErrNotFound
	}
	if err != nil {
		return AgentNode{}, fmt.Errorf("agent node lookup: %w", err)
	}
	return n, nil
}

// RevokeNode revokes one of the tenant's nodes and removes its membership,
// so the node can't act any more, and records who did it. Admins only
// (ErrNotAdmin; the function checks again). ErrNotFound for a node that
// isn't the tenant's. A node that is already revoked gives already=true
// and changes nothing, not even the audit.
//
// The node's live stream is the agent link's to cut (6a-2): call it after
// the transaction commits.
func (t *Tx) RevokeNode(ctx context.Context, nodeID, requestID string) (already bool, err error) {
	if err := t.requireAdmin(); err != nil {
		return false, err
	}
	if ids.Check(nodeID) != nil {
		return false, ErrNotFound
	}
	var result string
	if err := t.tx.QueryRow(ctx, `SELECT waystone.revoke_node($1)`, nodeID).Scan(&result); err != nil {
		return false, fmt.Errorf("revoke node: %w", err)
	}
	switch result {
	case "not_found":
		return false, ErrNotFound
	case "already_revoked":
		return true, nil
	case "revoked":
	default:
		return false, fmt.Errorf("revoke node: unexpected result %q", result)
	}
	err = t.insertAudit(ctx, Denial{
		Action:       "node.revoke",
		ResourceType: "node",
		ResourceID:   nodeID,
		Reason:       "revoked",
		RequestID:    requestID,
	}, "recorded")
	return false, err
}

// MaxCapabilities is how many capabilities a node may name.
const MaxCapabilities = 8

var (
	capabilityRE   = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	agentVersionRE = regexp.MustCompile(`^[\x20-\x7e]{1,64}$`)
)

// ValidAgentInfo reports whether an agent's version and capabilities may
// be stored: the version 1 to 64 printable ASCII bytes, at most
// MaxCapabilities capabilities, each 1 to 32 of a-z, 0-9 and '-'. The
// guard on nodes holds the same rules.
func ValidAgentInfo(version string, caps []string) bool {
	if !agentVersionRE.MatchString(version) || len(caps) > MaxCapabilities {
		return false
	}
	for _, c := range caps {
		if !capabilityRE.MatchString(c) {
			return false
		}
	}
	return true
}

// SetAgentInfo stores the version and capabilities the transaction's node
// said hello with (D4). Only the node itself (ErrNotNode). Invalid values
// are ErrInvalidInput.
func (t *Tx) SetAgentInfo(ctx context.Context, version string, caps []string) error {
	if err := t.requireNode(); err != nil {
		return err
	}
	if !ValidAgentInfo(version, caps) {
		return ErrInvalidInput
	}
	if caps == nil {
		caps = []string{}
	}
	tag, err := t.tx.Exec(ctx, `
		UPDATE waystone.nodes SET agent_version = $2, capabilities = $3
		WHERE node_id = $1`, t.actor.ID, version, caps)
	if err != nil {
		return fmt.Errorf("set agent info: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// validLabel: 1 to 200 bytes of UTF-8 without control characters, the
// rule insertAudit applies to every field.
func validLabel(s string) bool {
	if len(s) == 0 || len(s) > 200 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// canonicalIP: an IP address in the form netip prints it, at most 45
// bytes, as audit rows keep it (A6).
func canonicalIP(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && len(s) <= 45 && a.String() == s
}
