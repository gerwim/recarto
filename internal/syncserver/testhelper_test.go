package syncserver

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
)

// unixHTTPClient starts handler on a unix-domain-socket listener and returns
// an *http.Client wired to dial it, plus the base URL to use in requests.
//
// This sandbox's loopback TCP is unreachable (net.Listen("tcp", ...) followed
// by a Dial to the same address fails with "connection refused" even for a
// freshly bound port), so httptest.NewServer (which always binds
// 127.0.0.1:0) cannot be used here. A unix socket exercises the exact same
// net/http server and client code paths and is unaffected by that
// restriction.
func unixHTTPClient(t *testing.T, handler http.Handler) (*http.Client, string) {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}
	return client, "http://unix"
}
