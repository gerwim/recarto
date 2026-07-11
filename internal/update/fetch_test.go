package update

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
)

// unixHTTPClient starts handler on a unix-domain-socket listener. Some CI
// sandboxes make loopback TCP unreachable (a freshly net.Listen'd 127.0.0.1
// port refuses connections even from the same process), so httptest.Server
// cannot be used here; a unix socket exercises the identical net/http server
// and client machinery unaffected by that restriction. Duplicated per test
// package (no shared testutil package yet) — see internal/remotesync.
func unixHTTPClient(t *testing.T, handler http.Handler) *http.Client {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}
}

func TestHTTPFetcher(t *testing.T) {
	client := unixHTTPClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/recarto/index.json":
			io.WriteString(w, goldenManifest)
		case "/recarto/index.json.minisig":
			io.WriteString(w, "SIGDATA")
		case "/recarto/1.4.0/recarto-linux-amd64":
			io.WriteString(w, "BINARY")
		default:
			http.NotFound(w, r)
		}
	}))

	f := &HTTPFetcher{BaseURL: "http://recarto.test/", Client: client}
	ctx := context.Background()

	man, sig, err := f.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(man) != goldenManifest || string(sig) != "SIGDATA" {
		t.Fatalf("manifest/sig wrong: %q %q", man, sig)
	}

	rc, err := f.Binary(ctx, "recarto/1.4.0/recarto-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "BINARY" {
		t.Fatalf("binary body wrong: %q", b)
	}

	if _, err := f.Binary(ctx, "recarto/nope"); err == nil {
		t.Fatal("404 must error")
	}
}

func TestBaseURLOverride(t *testing.T) {
	t.Setenv("RECARTO_UPDATE_BASE_URL", "https://example.test/x/")
	if got := BaseURL(); got != "https://example.test/x/" {
		t.Fatalf("override ignored: %q", got)
	}
	t.Setenv("RECARTO_UPDATE_BASE_URL", "")
	if got := BaseURL(); got != DefaultBaseURL {
		t.Fatalf("default wrong: %q", got)
	}
}
