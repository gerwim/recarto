package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLSPCrossCheck validates the fixture's expectation markers against the
// language's OWN language server — the same authority an IDE's find-usages
// uses (csharp-ls is Roslyn-based, like Rider). For every `def` marker it asks
// `textDocument/references` (declarations included) at the symbol's name and
// enforces line-level agreement in both directions:
//
//	A. every location the LSP reports is a def/ref/tok-marked line — the
//	   fixture (and therefore recarto's grep contract) never MISSES a real
//	   usage;
//	B. every def/ref-marked line is reported by the LSP for some same-named
//	   definition — the fixture never CLAIMS a usage the compiler disagrees
//	   with (tok lines are exempt: textual-only occurrences).
//
// Together with the main suite (which proves recarto's answers equal the
// markers) this closes the loop: recarto == markers == LSP.
//
// The check needs the fixture language's server on PATH (manifest
// `lsp_command`); it skips — loudly — when absent, so plain `go test ./...`
// stays hermetic.
func TestLSPCrossCheck(t *testing.T) {
	if testing.Short() {
		t.Skip("LSP cross-check is slow; skipped in -short")
	}
	entries, err := filepath.Glob(filepath.Join("testdata", "*", "manifest.json"))
	if err != nil || len(entries) == 0 {
		t.Fatal("no language fixtures found")
	}
	for _, man := range entries {
		dir := filepath.Dir(man)
		t.Run(filepath.Base(dir), func(t *testing.T) {
			fx := loadFixture(t, dir)
			if len(fx.man.LSPCommand) == 0 {
				t.Skipf("fixture %s declares no lsp_command", dir)
			}
			if _, err := exec.LookPath(fx.man.LSPCommand[0]); err != nil {
				t.Skipf("%s not on PATH; install it to run the LSP cross-check", fx.man.LSPCommand[0])
			}
			runLSPCrossCheck(t, fx)
		})
	}
}

func runLSPCrossCheck(t *testing.T, fx *fixture) {
	// The server may write design-time build artifacts (obj/); never point it at
	// the checked-in testdata.
	wt := t.TempDir()
	copyTree(t, fx, wt)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cli := startLSP(t, ctx, fx.man.LSPCommand, wt)

	var initRes json.RawMessage
	if err := cli.call("initialize", map[string]any{
		"processId":    nil,
		"rootUri":      "file://" + wt,
		"capabilities": map[string]any{},
		"workspaceFolders": []map[string]any{
			{"uri": "file://" + wt, "name": "fixture"},
		},
	}, &initRes, 30*time.Second); err != nil {
		t.Fatalf("initialize: %v\nserver log:\n%s", err, cli.logTail())
	}
	cli.notify("initialized", map[string]any{})
	// Opening the sources nudges servers that lazy-load, and is required by some
	// to consider a file part of the workspace.
	for _, f := range fx.srcFiles {
		content := strings.Join(fx.lines[f], "\n") + "\n"
		cli.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri": "file://" + filepath.Join(wt, filepath.FromSlash(f)), "languageId": fx.lang,
				"version": 1, "text": content,
			},
		})
	}

	// Readiness: solution loading is asynchronous; poll a probe symbol until its
	// references span another project than its declaration (proof the whole
	// solution, not just one project, is queryable).
	probe := pickProbe(t, fx)
	probeDir := topDir(probe.File)
	deadline := time.Now().Add(3 * time.Minute)
	for {
		refs, err := cli.references(wt, fx, probe)
		crossProject := false
		for loc := range refs {
			if topDir(loc) != probeDir {
				crossProject = true
			}
		}
		if err == nil && crossProject {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never became ready (probe %s refs=%v err=%v)\nserver log:\n%s",
				probe.QN, refs, err, cli.logTail())
		}
		time.Sleep(2 * time.Second)
	}

	// covered[name] accumulates every line the LSP attributed to any definition
	// of that name (or its C# attribute-alias).
	covered := map[string]map[string]bool{}
	addCovered := func(name, loc string) {
		if covered[name] == nil {
			covered[name] = map[string]bool{}
		}
		covered[name][loc] = true
	}

	for _, d := range fx.defs {
		if d.Kind == "namespace" {
			// Namespaces are exempt for the same reason as in fixture hygiene: a
			// dotted name's textual matches are prefix-ambiguous, and a namespace's
			// reference set spans every using/declaration — not what grep models.
			continue
		}
		name := d.name()
		refs, err := cli.references(wt, fx, d)
		if err != nil {
			t.Fatalf("references(%s at %s): %v\nserver log:\n%s", d.QN, d.loc(), err, cli.logTail())
		}
		if len(refs) == 0 {
			t.Errorf("LSP returned no locations at all for %s (%s) — wrong name position?", d.QN, d.loc())
			continue
		}
		allowed := fx.markedLines(name, true)
		for l := range fx.implicitLines(name) {
			allowed[l] = true
		}
		names := []string{name}
		// C# attribute classes are used WITHOUT their "Attribute" suffix:
		// references(AuditAttribute) legitimately lands on `[Audit(...)]` lines.
		if alias := strings.TrimSuffix(name, "Attribute"); alias != name {
			for l := range fx.markedLines(alias, true) {
				allowed[l] = true
			}
			names = append(names, alias)
		}
		for loc := range refs {
			if !allowed[loc] {
				t.Errorf("LSP finds a reference of %s at %s that no def/ref/tok marker covers — the fixture (and grep's contract) MISSES a real usage", d.QN, loc)
			}
			for _, n := range names {
				addCovered(n, loc)
			}
		}
	}

	// Direction B: every semantically-marked line (refs, defs, and implicit
	// references) must be confirmed by the LSP for some same-named definition —
	// except documented server gaps, which must stay real to stay listed.
	for _, name := range fx.trackedNames() {
		if strings.Contains(name, ".") || covered[name] == nil {
			continue // namespaces, and names whose defs we could not query
		}
		want := fx.markedLines(name, false)
		for l := range fx.implicitLines(name) {
			want[l] = true
		}
		var missing []string
		for loc := range want {
			reason := fx.man.LSPKnownMisses[name][loc]
			switch {
			case covered[name][loc] && reason != "":
				t.Errorf("stale lsp_known_misses entry: the server now DOES report %q at %s (%s) — remove it from manifest.json", name, loc, reason)
			case !covered[name][loc] && reason == "":
				missing = append(missing, loc)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("marker claims %q is used at %s, but the LSP confirms no such reference — over-annotated fixture (use `tok` if it is textual-only)",
				name, strings.Join(missing, ", "))
		}
	}
}

// pickProbe selects a readiness probe: a unique definition whose marked usages
// span at least two top-level directories (projects).
func pickProbe(t *testing.T, fx *fixture) def {
	t.Helper()
	for _, d := range fx.defs {
		if d.Kind == "namespace" {
			continue
		}
		if len(fx.resolveDefs(d.name(), anyKind)) != 1 {
			continue
		}
		dirs := map[string]bool{}
		for loc := range fx.markedLines(d.name(), false) {
			dirs[topDir(loc)] = true
		}
		if len(dirs) >= 2 {
			return d
		}
	}
	t.Fatal("no suitable readiness probe (need a unique symbol used across two projects)")
	return def{}
}

func topDir(path string) string {
	if i := strings.Index(path, "/"); i > 0 {
		return path[:i]
	}
	return path
}

// --- minimal LSP (JSON-RPC 2.0 over stdio) client ---------------------------

type lspClient struct {
	t      *testing.T
	stdin  io.WriteCloser
	mu     sync.Mutex
	nextID int64
	wait   map[int64]chan lspResponse

	logMu sync.Mutex
	log   []string
}

type lspResponse struct {
	Result json.RawMessage
	Err    error
}

func startLSP(t *testing.T, ctx context.Context, command []string, dir string) *lspClient {
	t.Helper()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", command, err)
	}
	cli := &lspClient{t: t, stdin: stdin, wait: map[int64]chan lspResponse{}}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() && stderr.Len() > 0 {
			t.Logf("%s stderr:\n%s", command[0], tailString(stderr.String(), 2000))
		}
	})
	go cli.read(bufio.NewReaderSize(stdout, 1<<20))
	return cli
}

func (c *lspClient) read(r *bufio.Reader) {
	for {
		length := -1
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				c.failAll(fmt.Errorf("server closed stream: %w", err))
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if v, ok := strings.CutPrefix(line, "Content-Length: "); ok {
				fmt.Sscanf(v, "%d", &length)
			}
		}
		if length < 0 {
			c.failAll(fmt.Errorf("frame without Content-Length"))
			return
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(r, buf); err != nil {
			c.failAll(fmt.Errorf("short frame: %w", err))
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(buf, &msg); err != nil {
			continue
		}
		switch {
		case msg.Method == "" && msg.ID != nil: // response to one of our requests
			var id int64
			if json.Unmarshal(msg.ID, &id) == nil {
				c.mu.Lock()
				ch := c.wait[id]
				delete(c.wait, id)
				c.mu.Unlock()
				if ch != nil {
					resp := lspResponse{Result: msg.Result}
					if msg.Error != nil {
						resp.Err = fmt.Errorf("lsp error %d: %s", msg.Error.Code, msg.Error.Message)
					}
					ch <- resp
				}
			}
		case msg.Method != "" && msg.ID != nil: // server-to-client request: answer politely
			result := any(nil)
			if msg.Method == "workspace/configuration" {
				var p struct {
					Items []json.RawMessage `json:"items"`
				}
				_ = json.Unmarshal(msg.Params, &p)
				result = make([]any, len(p.Items))
			}
			c.send(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(msg.ID), "result": result})
		case msg.Method == "window/logMessage" || msg.Method == "window/showMessage":
			var p struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			c.logMu.Lock()
			c.log = append(c.log, p.Message)
			c.logMu.Unlock()
		}
	}
}

func (c *lspClient) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.wait {
		ch <- lspResponse{Err: err}
		delete(c.wait, id)
	}
}

func (c *lspClient) send(msg map[string]any) {
	raw, err := json.Marshal(msg)
	if err != nil {
		c.t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n%s", len(raw), raw)
}

func (c *lspClient) notify(method string, params any) {
	c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *lspClient) call(method string, params, out any, timeout time.Duration) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan lspResponse, 1)
	c.wait[id] = ch
	c.mu.Unlock()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	c.mu.Lock()
	fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n%s", len(raw), raw)
	c.mu.Unlock()
	select {
	case resp := <-ch:
		if resp.Err != nil {
			return resp.Err
		}
		if out != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	case <-time.After(timeout):
		c.mu.Lock()
		delete(c.wait, id)
		c.mu.Unlock()
		return fmt.Errorf("%s timed out after %s", method, timeout)
	}
}

func (c *lspClient) logTail() string {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	return tailString(strings.Join(c.log, "\n"), 3000)
}

func tailString(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

// references asks for d's references at its declared name token and returns
// fixture-relative "path:line" locations (1-based lines).
func (c *lspClient) references(wt string, fx *fixture, d def) (map[string]bool, error) {
	line := fx.lines[d.File][d.Line-1]
	code := fx.stripMarker(line)
	m := regexp.MustCompile(wordPattern(d.name())).FindStringIndex(code)
	if m == nil {
		return nil, fmt.Errorf("name %q not on its own declaration line %s", d.name(), d.loc())
	}
	var locs []struct {
		URI   string `json:"uri"`
		Range struct {
			Start struct {
				Line int `json:"line"`
			} `json:"start"`
		} `json:"range"`
	}
	err := c.call("textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": "file://" + filepath.Join(wt, filepath.FromSlash(d.File))},
		"position":     map[string]any{"line": d.Line - 1, "character": m[0]},
		"context":      map[string]any{"includeDeclaration": true},
	}, &locs, 30*time.Second)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, l := range locs {
		p := strings.TrimPrefix(l.URI, "file://")
		rel, rerr := filepath.Rel(wt, filepath.FromSlash(p))
		if rerr != nil || strings.HasPrefix(rel, "..") {
			continue // outside the fixture (decompiled metadata etc.)
		}
		out[fmt.Sprintf("%s:%d", filepath.ToSlash(rel), l.Range.Start.Line+1)] = true
	}
	return out, nil
}
