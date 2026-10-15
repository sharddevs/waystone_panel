// SPDX-License-Identifier: AGPL-3.0-only

package minecraft

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeServer listens on loopback and runs serve for each connection.
func fakeServer(t *testing.T, serve func(c net.Conn)) uint32 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				serve(c)
			}()
		}
	}()
	return uint32(l.Addr().(*net.TCPAddr).Port)
}

// handshake is what the server reads from a client.
type handshake struct {
	protocol uint64
	host     string
	port     uint16
	next     uint64
	request  []byte // the status request packet after the handshake
}

// readHandshake parses the client's two packets exactly.
func readHandshake(r *bufio.Reader) (handshake, error) {
	var h handshake
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return h, err
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return h, err
	}
	pr := bytes.NewReader(p)
	if id, _ := binary.ReadUvarint(pr); id != 0 {
		return h, errors.New("handshake ID")
	}
	h.protocol, _ = binary.ReadUvarint(pr)
	hl, _ := binary.ReadUvarint(pr)
	host := make([]byte, hl)
	if _, err := io.ReadFull(pr, host); err != nil {
		return h, err
	}
	h.host = string(host)
	if err := binary.Read(pr, binary.BigEndian, &h.port); err != nil {
		return h, err
	}
	h.next, _ = binary.ReadUvarint(pr)
	if pr.Len() != 0 {
		return h, errors.New("trailing bytes in handshake")
	}
	h.request = make([]byte, 2)
	_, err = io.ReadFull(r, h.request)
	return h, err
}

// packet frames a status response holding body.
func packet(body string) []byte { return framed(0, uint64(len(body)), body) }

// framed builds a packet with the given ID and declared string length.
func framed(id, size uint64, body string) []byte {
	var p []byte
	p = binary.AppendUvarint(p, id)
	p = binary.AppendUvarint(p, size)
	p = append(p, body...)
	return append(binary.AppendUvarint(nil, uint64(len(p))), p...)
}

const goodStatus = `{"version":{"name":"1.21.10","protocol":773},"players":{"max":20,"online":0},"description":{"text":"A Minecraft Server"}}`

func answer(resp []byte, got chan<- handshake) func(net.Conn) {
	return func(c net.Conn) {
		h, err := readHandshake(bufio.NewReader(c))
		if err != nil {
			return
		}
		if got != nil {
			got <- h
		}
		c.Write(resp)
	}
}

func probe(port uint32) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return StatusPing{}.Probe(ctx, port)
}

// The client sends a correct handshake (protocol -1, host, port, next
// state 1) and status request, and a real status reply is ready.
func TestStatusPingReady(t *testing.T) {
	got := make(chan handshake, 1)
	port := fakeServer(t, answer(packet(goodStatus), got))
	if err := probe(port); err != nil {
		t.Fatalf("Probe = %v, want nil", err)
	}
	h := <-got
	if h.protocol != 0xFFFFFFFF || h.host != "127.0.0.1" || uint32(h.port) != port || h.next != 1 ||
		!bytes.Equal(h.request, []byte{1, 0}) {
		t.Errorf("handshake = %+v", h)
	}
}

// Anything short of a real status reply is not ready.
func TestStatusPingNotReady(t *testing.T) {
	long := append(binary.AppendUvarint(nil, maxStatusBytes+1), 0)
	cases := map[string][]byte{
		"closed at once":     nil,
		"no version":         packet(`{"players":{"max":20}}`),
		"version not object": packet(`{"version":"1.21"}`),
		"not JSON":           packet(`hello`),
		"wrong packet ID":    framed(1, uint64(len(goodStatus)), goodStatus),
		"string too long":    framed(0, uint64(len(goodStatus))+1, goodStatus),
		"string too short":   framed(0, uint64(len(goodStatus))-1, goodStatus),
		"oversized length":   long,
		"zero length":        {0},
		"truncated":          packet(goodStatus)[:20],
		"bad varint":         {0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
	}
	for name, resp := range cases {
		port := fakeServer(t, answer(resp, nil))
		if err := probe(port); err == nil {
			t.Errorf("%s: ready, want an error", name)
		}
	}
}

// A declared length over the cap is refused before anything is read or
// allocated, even when the bytes that follow are a valid status reply.
func TestStatusPingOversized(t *testing.T) {
	body := `{"version":{"name":"1.21.10","protocol":773},"favicon":"` + strings.Repeat("x", maxStatusBytes) + `"}`
	resp := framed(0, uint64(len(body)), body)
	port := fakeServer(t, answer(resp, nil))
	if err := probe(port); !errors.Is(err, ErrBadStatus) {
		t.Errorf("oversized reply: %v, want ErrBadStatus", err)
	}
}

// Nothing listening: not ready.
func TestStatusPingRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint32(l.Addr().(*net.TCPAddr).Port)
	l.Close()
	if err := probe(port); err == nil {
		t.Error("ready with nothing listening")
	}
}

// A server that accepts but never answers is cut off at the deadline;
// a TCP connect alone isn't readiness.
func TestStatusPingSilent(t *testing.T) {
	port := fakeServer(t, func(c net.Conn) { io.Copy(io.Discard, c) })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := (StatusPing{}).Probe(ctx, port); err == nil {
		t.Error("ready from a silent server")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %s, want about the 100 ms deadline", d)
	}
}

// Cancelling ctx cuts a silent server off at once, long before the
// deadline.
func TestStatusPingCancel(t *testing.T) {
	port := fakeServer(t, func(c net.Conn) { io.Copy(io.Discard, c) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	if err := (StatusPing{}).Probe(ctx, port); err == nil {
		t.Error("ready from a silent server")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %s after cancel, want about 100 ms", d)
	}
}

// Without a deadline on ctx, the probe sets its own.
func TestStatusPingDefaultDeadline(t *testing.T) {
	var deadline time.Time
	p := StatusPing{Dial: func(ctx context.Context, address string) (net.Conn, error) {
		deadline, _ = ctx.Deadline()
		return nil, errors.New("no dial in this test")
	}}
	before := time.Now()
	if err := p.Probe(context.Background(), 25565); err == nil {
		t.Fatal("dial error ignored")
	}
	if d := deadline.Sub(before); d < pingTimeout-time.Second || d > pingTimeout+time.Second {
		t.Errorf("deadline in %s, want about %s", d, pingTimeout)
	}
}

func TestStatusPingHostAndPortRange(t *testing.T) {
	var addr string
	p := StatusPing{Host: "::1", Dial: func(_ context.Context, a string) (net.Conn, error) {
		addr = a
		return nil, errors.New("no dial")
	}}
	_ = p.Probe(context.Background(), 25565)
	if addr != "[::1]:25565" {
		t.Errorf("dialed %q", addr)
	}
	for _, port := range []uint32{0, 65536} {
		addr = ""
		if err := p.Probe(context.Background(), port); err == nil || addr != "" {
			t.Errorf("port %d: err %v, dialed %q", port, err, addr)
		}
	}
}

func TestAppendVarInt(t *testing.T) {
	if got := appendVarInt(nil, -1); !bytes.Equal(got, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x0F}) {
		t.Errorf("-1 = % x", got)
	}
	if got := appendVarInt(nil, 300); !bytes.Equal(got, []byte{0xAC, 0x02}) {
		t.Errorf("300 = % x", got)
	}
}
