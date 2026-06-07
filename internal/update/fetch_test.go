package update

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPFetcher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer srv.Close()

	f := &HTTPFetcher{BaseURL: srv.URL + "/", Client: srv.Client()}
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
