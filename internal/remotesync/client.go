package remotesync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/syncwire"
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("recarto sync: healthz: %s", resp.Status)
	}
	return nil
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+c.Key)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, fmt.Errorf("recarto sync: %s %s: %s", req.Method, req.URL.Path, resp.Status)
	}
	return resp, nil
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

// Push sends locally-authored records (including tombstones) to the server.
func (c *Client) Push(ctx context.Context, repoKey string, records []syncwire.Record) (syncwire.PushResponse, error) {
	body, err := json.Marshal(syncwire.PushRequest{Records: records})
	if err != nil {
		return syncwire.PushResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.changesURL(repoKey), bytes.NewReader(body))
	if err != nil {
		return syncwire.PushResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
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
