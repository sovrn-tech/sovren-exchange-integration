package main

// Track A regression test for PR #300 finding #5: the metrics/admin listeners
// must bind SYNCHRONOUSLY so a bind failure is fatal at startup rather than a
// goroutine-only log while custody workers run with no operational surface.

import (
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
)

// TestServeHTTPBindFailureIsSynchronous: an unbindable address returns an error
// from serveHTTP itself (no server handle), so main can treat it as fatal
// before any worker starts. 203.0.113.0/24 is RFC 5737 documentation space and
// is not assignable to a local interface, so the bind reliably fails.
func TestServeHTTPBindFailureIsSynchronous(t *testing.T) {
	srv, err := serveHTTP("203.0.113.1:9", "127.0.0.1:0", http.NewServeMux(), logging.New("test"), "metrics")
	require.Error(t, err)
	require.Nil(t, srv)
}

// TestServeHTTPBindsBeforeReturning: a bindable address returns a live server
// handle and the port is actually held (a second bind on the same address
// fails), proving Serve() runs on an already-bound listener.
func TestServeHTTPBindsBeforeReturning(t *testing.T) {
	srv, err := serveHTTP("127.0.0.1:0", "127.0.0.1:0", http.NewServeMux(), logging.New("test"), "metrics")
	require.NoError(t, err)
	require.NotNil(t, srv)
	t.Cleanup(func() { _ = srv.Close() })

	require.NotEmpty(t, srv.Addr)
	ln, err := net.Listen("tcp", srv.Addr)
	require.Error(t, err, "serveHTTP must already hold the bound port")
	if ln != nil {
		_ = ln.Close()
	}
}

// TestServeHTTPEmptyListenUsesFallback: an empty listen binds the loopback
// fallback rather than failing.
func TestServeHTTPEmptyListenUsesFallback(t *testing.T) {
	srv, err := serveHTTP("", "127.0.0.1:0", http.NewServeMux(), logging.New("test"), "admin")
	require.NoError(t, err)
	require.NotNil(t, srv)
	t.Cleanup(func() { _ = srv.Close() })
}
