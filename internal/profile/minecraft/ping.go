// SPDX-License-Identifier: AGPL-3.0-only

package minecraft

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// The Minecraft profile's readiness probe (stage 4d-2, D6). It does what a
// game client does before it joins: the server list ping. A server that
// answers it with a proper status reply is ready for players. A TCP
// connect alone isn't enough (the port opens before the world is loaded
// on some versions and proxies), and the "Done" log line is never used.
//
// Protocol (https://minecraft.wiki/w/Java_Edition_protocol/Server_List_Ping):
// every packet is a VarInt length, a VarInt packet ID, then the fields.
// The client sends a handshake (ID 0: protocol version, address, port,
// next state 1 = status) and a status request (ID 0, no fields); the
// server answers with a status response (ID 0: one string holding JSON).

var (
	// ErrBadStatus: something answered on the port, but not with a
	// Minecraft status response.
	ErrBadStatus = errors.New("not a Minecraft status response")
)

const (
	// pingTimeout bounds the whole exchange when ctx has no deadline.
	pingTimeout = 3 * time.Second
	// maxStatusBytes caps the response. The JSON is usually under 1 KiB,
	// but a server icon adds up to about 20 KiB.
	maxStatusBytes = 256 << 10
	// protocolAny is -1: "I don't know the version", which every server
	// answers the status of.
	protocolAny = -1
	// stateStatus is the handshake's next state for a status request.
	stateStatus = 1
)

// StatusPing probes a server on this host. The zero value is ready to use.
type StatusPing struct {
	// Host is where the server listens; 127.0.0.1 when empty. The agent
	// runs on the server's own host.
	Host string
	// Dial is for tests; nil means a TCP dial.
	Dial func(ctx context.Context, address string) (net.Conn, error)
}

// Probe returns nil when the server on port answers a status ping with a
// status reply that names its version, and an error otherwise.
func (p StatusPing) Probe(ctx context.Context, port uint32) error {
	if port == 0 || port > 65535 {
		return fmt.Errorf("port %d out of range", port)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pingTimeout)
		defer cancel()
	}
	host := p.Host
	if host == "" {
		host = "127.0.0.1"
	}
	dial := p.Dial
	if dial == nil {
		dial = func(ctx context.Context, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", address)
		}
	}
	conn, err := dial(ctx, net.JoinHostPort(host, strconv.FormatUint(uint64(port), 10)))
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	// Close the connection if ctx ends before the deadline does.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	if _, err := conn.Write(statusRequest(host, uint16(port))); err != nil {
		return err
	}
	return readStatus(bufio.NewReader(conn))
}

// statusRequest builds the handshake and the status request, back to back.
func statusRequest(host string, port uint16) []byte {
	var hs []byte
	hs = binary.AppendUvarint(hs, 0) // packet ID: handshake
	hs = appendVarInt(hs, protocolAny)
	hs = binary.AppendUvarint(hs, uint64(len(host)))
	hs = append(hs, host...)
	hs = binary.BigEndian.AppendUint16(hs, port)
	hs = binary.AppendUvarint(hs, stateStatus)

	var out []byte
	out = binary.AppendUvarint(out, uint64(len(hs)))
	out = append(out, hs...)
	out = binary.AppendUvarint(out, 1) // length of the status request
	out = binary.AppendUvarint(out, 0) // packet ID: status request
	return out
}

// appendVarInt appends v as Minecraft's VarInt: a signed 32-bit value sent
// as its unsigned bits, so -1 takes five bytes.
func appendVarInt(b []byte, v int32) []byte {
	return binary.AppendUvarint(b, uint64(uint32(v)))
}

// readStatus reads one status response and checks it.
func readStatus(r *bufio.Reader) error {
	n, err := readVarInt(r)
	if err != nil {
		return err
	}
	if n < 1 || n > maxStatusBytes {
		return fmt.Errorf("%w: packet length %d", ErrBadStatus, n)
	}
	packet := make([]byte, n)
	if _, err := io.ReadFull(r, packet); err != nil {
		return err
	}
	pr := bytes.NewReader(packet)
	id, err := readVarInt(pr)
	if err != nil || id != 0 {
		return fmt.Errorf("%w: packet ID", ErrBadStatus)
	}
	size, err := readVarInt(pr)
	if err != nil || size != uint64(pr.Len()) {
		return fmt.Errorf("%w: string length", ErrBadStatus)
	}
	var status struct {
		Version *struct {
			Name     string `json:"name"`
			Protocol int    `json:"protocol"`
		} `json:"version"`
	}
	rest := packet[len(packet)-pr.Len():]
	if err := json.Unmarshal(rest, &status); err != nil || status.Version == nil {
		return fmt.Errorf("%w: no version in the reply", ErrBadStatus)
	}
	return nil
}

// readVarInt reads an unsigned VarInt of at most five bytes (32 bits).
func readVarInt(r io.ByteReader) (uint64, error) {
	v, err := binary.ReadUvarint(r)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return 0, io.ErrUnexpectedEOF
		}
		return 0, fmt.Errorf("%w: %v", ErrBadStatus, err)
	}
	if v > 0xFFFFFFFF {
		return 0, fmt.Errorf("%w: VarInt too long", ErrBadStatus)
	}
	return v, nil
}
