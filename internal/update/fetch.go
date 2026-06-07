package update

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultBaseURL is the public distribution root. Artifact paths in the manifest
// are joined onto this. Overridable via RECARTO_UPDATE_BASE_URL (used by tests).
const DefaultBaseURL = "https://releases.gerwim.com/"

const manifestPath = "recarto/index.json"

// BaseURL returns the distribution root, honoring the RECARTO_UPDATE_BASE_URL
// override. The returned value always ends in "/".
func BaseURL() string {
	if v := os.Getenv("RECARTO_UPDATE_BASE_URL"); v != "" {
		if !strings.HasSuffix(v, "/") {
			v += "/"
		}
		return v
	}
	return DefaultBaseURL
}

// Fetcher retrieves the signed manifest and individual binaries.
type Fetcher interface {
	Manifest(ctx context.Context) (manifest, signature []byte, err error)
	Binary(ctx context.Context, path string) (io.ReadCloser, error)
}

// HTTPFetcher fetches over HTTPS from BaseURL.
type HTTPFetcher struct {
	BaseURL string
	Client  *http.Client
}

// NewHTTPFetcher builds a fetcher against the resolved BaseURL with a sane timeout.
func NewHTTPFetcher() *HTTPFetcher {
	return &HTTPFetcher{BaseURL: BaseURL(), Client: &http.Client{Timeout: 60 * time.Second}}
}

func (f *HTTPFetcher) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return resp, nil
}

// Manifest downloads index.json and its detached signature.
func (f *HTTPFetcher) Manifest(ctx context.Context) ([]byte, []byte, error) {
	mResp, err := f.get(ctx, manifestPath)
	if err != nil {
		return nil, nil, err
	}
	defer mResp.Body.Close()
	manifest, err := io.ReadAll(mResp.Body)
	if err != nil {
		return nil, nil, err
	}
	sResp, err := f.get(ctx, manifestPath+".minisig")
	if err != nil {
		return nil, nil, err
	}
	defer sResp.Body.Close()
	sig, err := io.ReadAll(sResp.Body)
	if err != nil {
		return nil, nil, err
	}
	return manifest, sig, nil
}

// Binary streams the binary at the given manifest-relative path. Caller closes it.
func (f *HTTPFetcher) Binary(ctx context.Context, path string) (io.ReadCloser, error) {
	resp, err := f.get(ctx, path)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
