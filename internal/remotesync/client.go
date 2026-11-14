package remotesync

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gerwim/recarto/internal/syncwire"
)

// Client is a thin HTTP client for the sync server's /v1/repos/{key}/changes
// API, mirroring internal/update/fetch.go's conventions (context-aware,
// bounded timeout, explicit status-code check).
type Client struct {
	BaseURL string
	Key     string
	HTTP    *http.Client
}

func (c *Client) changesURL(repoKey string) string {
	return strings.TrimRight(c.BaseURL, "/") + "/v1/repos/" + url.PathEscape(repoKey) + "/changes"
}

// Healthz checks the server's unauthenticated /healthz endpoint (for
// `recarto doctor`'s reachability check).
func (c *Client) Healthz(ctx context.Context) error {
	u := strings.TrimRight(c.BaseURL, "/") + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	// Only the status is read; closing the body cannot change the answer.
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("recarto sync: healthz: %s", resp.Status)
	}
	return nil
}

// do sends req, always advertising gzip support and transparently
// decompressing a gzip response body. Explicitly setting Accept-Encoding
// (rather than relying on Go's default transport-level auto-gzip) makes the
// behavior deterministic and testable, and lets Push symmetrically
// gzip-compress the request body — something the transport never does on its
// own.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close() // the failed status is the error to report
		return nil, fmt.Errorf("recarto sync: %s %s: %s", req.Method, req.URL.Path, resp.Status)
	}
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzr, err := gzip.NewReader(resp.Body)
		if err != nil {
			_ = resp.Body.Close() // the gzip error is the one to report
			return nil, err
		}
		resp.Body = gzipRespBody{gz: gzr, underlying: resp.Body}
	}
	return resp, nil
}

// gzipRespBody closes both the gzip reader and the underlying HTTP body.
type gzipRespBody struct {
	gz         *gzip.Reader
	underlying io.ReadCloser
}

func (g gzipRespBody) Read(p []byte) (int, error) { return g.gz.Read(p) }
func (g gzipRespBody) Close() error {
	gerr := g.gz.Close()
	uerr := g.underlying.Close()
	if gerr != nil {
		return gerr
	}
	return uerr
}

// gzipEncode gzip-compresses data for a request body.
func gzipEncode(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Pull fetches records with server seq > since, up to limit.
func (c *Client) Pull(ctx context.Context, repoKey string, since int64, limit int) (syncwire.ChangesResponse, error) {
	u := c.changesURL(repoKey) + "?since=" + strconv.FormatInt(since, 10) + "&limit=" + strconv.Itoa(limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return syncwire.ChangesResponse{}, err
	}
	resp, err := c.do(req)
	if err != nil {
		return syncwire.ChangesResponse{}, err
	}
	defer resp.Body.Close()
	var out syncwire.ChangesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return syncwire.ChangesResponse{}, err
	}
	return out, nil
}

// Push sends locally-authored records (including tombstones) to the server,
// gzip-compressed (sync payloads are text-only memory rows and compress
// well — see internal/syncwire's Deleted comment for what's excluded from
// the wire; the code graph never travels).
func (c *Client) Push(ctx context.Context, repoKey string, records []syncwire.Record) (syncwire.PushResponse, error) {
	body, err := json.Marshal(syncwire.PushRequest{Records: records})
	if err != nil {
		return syncwire.PushResponse{}, err
	}
	gz, err := gzipEncode(body)
	if err != nil {
		return syncwire.PushResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.changesURL(repoKey), bytes.NewReader(gz))
	if err != nil {
		return syncwire.PushResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := c.do(req)
	if err != nil {
		return syncwire.PushResponse{}, err
	}
	defer resp.Body.Close()
	var out syncwire.PushResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return syncwire.PushResponse{}, err
	}
	return out, nil
}

// Import uploads a bulk, multi-project export archive (see
// internal/fullexport) to the server's POST /v1/import endpoint — the
// one-shot bulk migration path for a machine's pre-existing history,
// distinct from Push (which sends only the local outbox of one repo). gz
// must already be a gzip-compressed recarto-export/v1 stream (fullexport
// writes directly to a gzip.Writer; the CLI just reopens that file and
// streams it here as-is) — unlike Push, Import never buffers or re-encodes
// the body, since a full-history archive can be far larger than a single
// push batch.
func (c *Client) Import(ctx context.Context, gz io.Reader) (syncwire.ImportResponse, error) {
	u := strings.TrimRight(c.BaseURL, "/") + "/v1/import"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, gz)
	if err != nil {
		return syncwire.ImportResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := c.do(req)
	if err != nil {
		return syncwire.ImportResponse{}, err
	}
	defer resp.Body.Close()
	var out syncwire.ImportResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return syncwire.ImportResponse{}, err
	}
	return out, nil
}
