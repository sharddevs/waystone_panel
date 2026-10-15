// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"context"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/sharddevs/waystone_panel/internal/peercred"
)

// Server serves a Handler on web.sock. Only Handler.NewServer makes one.
type Server struct {
	http     *http.Server
	listener net.Listener
}

// NewServer returns the server for web.sock (l). Every accepted
// connection is checked with the kernel (SO_PEERCRED) and closed at once
// unless it comes from the proxy's uid (W2), before a byte of it is read.
// The limits match the local API's (I12).
func (h *Handler) NewServer(l net.Listener) *Server {
	return h.newServer(l, peercred.UID)
}

func (h *Handler) newServer(l net.Listener, uidOf func(net.Conn) (uint32, error)) *Server {
	return &Server{
		listener: &proxyListener{Listener: l, uid: h.proxy, uidOf: uidOf, log: h.log},
		http: &http.Server{
			Handler:           h,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    16 << 10,
			ErrorLog:          h.log,
		},
	}
}

// Serve serves until Shutdown or Close; then it returns
// http.ErrServerClosed.
func (s *Server) Serve() error { return s.http.Serve(s.listener) }

// Shutdown stops accepting and waits for in-flight requests, until ctx
// ends.
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

// Close cuts every connection off at once.
func (s *Server) Close() error { return s.http.Close() }

// Addr is the socket's address, for the startup log.
func (s *Server) Addr() net.Addr { return s.listener.Addr() }

// proxyListener only hands out connections from the proxy's uid. Any
// other caller (a local user, the agent, root) is logged and closed
// before the HTTP server sees it, so nothing can reach the guard but the
// proxy. A connection whose peer can't be determined is refused too.
type proxyListener struct {
	net.Listener
	uid   uint32
	uidOf func(net.Conn) (uint32, error)
	log   *log.Logger
}

func (l *proxyListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, err := l.uidOf(c)
		if err == nil && uid == l.uid {
			return c, nil
		}
		if err != nil {
			l.log.Printf("web refused a connection: no peer credentials: %v", err)
		} else {
			l.log.Printf("web refused a connection from uid %d (only the proxy, uid %d, may connect)", uid, l.uid)
		}
		c.Close()
	}
}
