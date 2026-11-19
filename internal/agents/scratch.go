package agents

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/features"
	"github.com/gerwim/recarto/internal/gitctx"
	"github.com/gerwim/recarto/internal/graph"
	"github.com/gerwim/recarto/internal/ledger"
	"github.com/gerwim/recarto/internal/memory"
	"github.com/gerwim/recarto/internal/store"
)

// rosterMetric prefixes a conversation's roster entries: one per repo it
// covers, valued by its 1-based position.
const rosterMetric = "roster:"

// rosterScope is the ledger scope a conversation's roster lives in: its main
// agent's, whose roster entries a compact or clear does not forget.
func rosterScope(sessionID string) string { return "session:" + sessionID }

// writeRoster records wts as sessionID's roster, in order.
func writeRoster(ctx context.Context, c store.Conn, sessionID string, wts []string) error {
	for i, wt := range wts {
		if _, err := c.ExecContext(ctx,
			`INSERT INTO counters(scope, metric, value) VALUES (?, ?, ?)
			 ON CONFLICT(scope, metric) DO NOTHING`,
			rosterScope(sessionID), rosterMetric+wt, i+1); err != nil {
			return err
		}
	}
	return nil
}

// readRoster returns sessionID's roster in order, nil when it has none.
func readRoster(ctx context.Context, l *ledger.Ledger, sessionID string) ([]string, error) {
	metrics, err := l.Metrics(ctx, rosterScope(sessionID), rosterMetric)
	if err != nil {
		return nil, err
	}
	type entry struct {
		wt  string
		pos int64
	}
	entries := make([]entry, 0, len(metrics))
	for _, m := range metrics {
		pos, err := l.Get(ctx, rosterScope(sessionID), m)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry{strings.TrimPrefix(m, rosterMetric), pos})
	}
	slices.SortFunc(entries, func(x, y entry) int { return int(x.pos - y.pos) })
	var out []string
	for _, e := range entries {
		out = append(out, e.wt)
	}
	return out, nil
}

// insertSession records session id in worktree g, reporting whether this call
// wrote it.
func insertSession(ctx context.Context, c store.Conn, id, agent string, g gitctx.Context,
	startedAt int64, endedAt sql.NullInt64) (bool, error) {
	res, err := c.ExecContext(ctx,
		`INSERT INTO sessions(id, agent, branch, worktree, started_at, ended_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		id, agent, nullable(g.Branch), g.Worktree, startedAt, endedAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// folderSession reads the start and agent of sessionID's row in st.
func folderSession(ctx context.Context, st *store.Store, sessionID string) (startedAt int64, agent string, found bool, err error) {
	err = st.DB.QueryRowContext(ctx,
		`SELECT started_at, agent FROM sessions WHERE id=?`, sessionID).Scan(&startedAt, &agent)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	return startedAt, agent, err == nil, err
}

// hasSession reports whether st holds a row for sessionID.
func hasSession(ctx context.Context, st *store.Store, sessionID string) (bool, error) {
	var ok bool
	err := st.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id=?)`, sessionID).Scan(&ok)
	return ok, err
}

// conversationApp returns a with the store and ledger of the folder store that
// recorded sessionID's conversation with a roster, when the folder has since
// become part of a git checkout; else a itself. A conversation started in a
// code-less folder keeps one record, in that folder's store, whatever the
// folder later becomes. done closes what it opened.
func conversationApp(ctx context.Context, a *app.App, sessionID string) (*app.App, func(), error) {
	noop := func() {}
	if sessionID == "" || a.Git.NonGit || a.Dir == "" {
		return a, noop, nil
	}
	if known, err := hasSession(ctx, a.Store, sessionID); err != nil || known {
		return a, noop, err
	}
	st, err := app.FolderStore(a.Dir)
	if err != nil || st == nil {
		return a, noop, err
	}
	led := ledger.New(st)
	found, err := hasSession(ctx, st, sessionID)
	var roster []string
	if err == nil && found {
		roster, err = readRoster(ctx, led, sessionID)
	}
	if err != nil || len(roster) == 0 {
		_ = st.Close()
		return a, noop, err
	}
	c := *a
	c.Store, c.Ledger = st, led
	return &c, func() { _ = st.Close() }, nil
}

// conversationStart is what a session start knows of its conversation.
type conversationStart struct {
	roster    []string // the repos it covers, in order; nil: the block of the session's own worktree
	first     bool     // this start is the first Recarto sees of the conversation
	startedAt int64    // the folder session's start
	agent     string   // the folder session's agent, for its roster repo sessions
}

// startConversation records ev's conversation at its first session start and
// returns what the start knows of it. The roster is written with the
// conversation's row, before any repo gets a session of it, so no repo outside
// the saved roster ever gets one.
func startConversation(ctx context.Context, a *app.App, ev Event, now int64, wait *startWait) (conversationStart, error) {
	if ev.SessionID == "" {
		conv := conversationStart{first: true, startedAt: now, agent: agentOrDefault(ev.Agent)}
		if codeless(a, wait) {
			conv.roster = repoWorktrees(a.Repos)
		}
		return conv, nil
	}
	known := func() (conversationStart, error) {
		startedAt, agent, _, err := folderSession(ctx, a.Store, ev.SessionID)
		if err != nil {
			return conversationStart{}, err
		}
		roster, err := readRoster(ctx, a.Ledger, ev.SessionID)
		return conversationStart{roster: roster, startedAt: startedAt, agent: agent}, err
	}
	// Read first: a known conversation needs no write, so its resume never
	// waits for another process's writer.
	found, err := hasSession(ctx, a.Store, ev.SessionID)
	if err != nil {
		return conversationStart{}, err
	}
	if found {
		return known()
	}
	var roster []string
	if codeless(a, wait) {
		roster = repoWorktrees(a.Repos)
	}
	agent := agentOrDefault(ev.Agent)
	var inserted bool
	if err := a.Store.WriteTx(ctx, func(c store.Conn) error {
		var err error
		if inserted, err = insertSession(ctx, c, ev.SessionID, agent, a.Git, now, sql.NullInt64{}); err != nil || !inserted || roster == nil {
			return err
		}
		return writeRoster(ctx, c, ev.SessionID, roster)
	}); err != nil {
		return conversationStart{}, err
	}
	if !inserted {
		// Another process recorded the conversation first; its roster counts.
		return known()
	}
	return conversationStart{roster: roster, first: true, startedAt: now, agent: agent}, nil
}

// codeless reports whether a's session folder is a code-less folder serving
// repos: outside git, with RECARTO_REPOS serving at least one, and holding no
// file an index would take in. A listing that fails or runs out of time
// proves nothing, so the folder then counts as holding code.
func codeless(a *app.App, wait *startWait) bool {
	if !a.Git.NonGit || len(a.Repos) == 0 {
		return false
	}
	code, err := graph.HasCode(wait.get(), a.Git.Worktree, true)
	return err == nil && !code
}

// repoWorktrees returns the repos' worktrees, in order.
func repoWorktrees(repos []*app.RepoContext) []string {
	out := make([]string, 0, len(repos))
	for _, rc := range repos {
		out = append(out, rc.Git.Worktree)
	}
	return out
}

// coveredRepo is a roster repo a block covers.
type coveredRepo struct {
	*app.RepoContext
	served bool   // RECARTO_REPOS serves it now: it gets the first-index wait
	sel    string // its selector; "" when the repo argument cannot reach it
	mark   string // "" with a selector, else why it has none
}

// Marks of a covered repo the repo argument cannot reach.
const (
	markUnserved     = "no longer served"
	markUnselectable = "the repo argument cannot select it"
)

// label is how the block names the repo: its selector, or its path and mark.
func (c coveredRepo) label() string {
	if c.sel != "" {
		return c.sel
	}
	return c.Git.Worktree + " (" + c.mark + ")"
}

// subject is the one-repo block's subject for c, in the conversation session
// started at since.
func (c coveredRepo) subject(session string, since int64) subject {
	s := subject{rc: c.RepoContext, served: c.served, session: session, since: since}
	if c.sel != "" {
		s.tools, s.header, s.reposLine = true, memoryHeader, alsoServing([]string{c.sel})
		return s
	}
	s.header = "Recarto memory for " + c.label() + ":\n"
	s.reposLine = "\nRepo: " + c.Git.Worktree + " (" + c.mark + "; graph and memory tools cannot reach it).\n"
	return s
}

// cover returns the roster repos a block covers, in roster order, and the
// worktrees of those that cannot be opened; done closes what cover opened.
func cover(a *app.App, roster []string) (covered []coveredRepo, gone []string, done func()) {
	var opened []*app.RepoContext
	for _, wt := range roster {
		if i := slices.IndexFunc(a.Repos, func(rc *app.RepoContext) bool { return rc.Git.Worktree == wt }); i >= 0 {
			rc := a.Repos[i]
			if sel, ok := a.Selector(rc); ok {
				covered = append(covered, coveredRepo{rc, true, sel, ""})
			} else {
				covered = append(covered, coveredRepo{rc, true, "", markUnselectable})
			}
			continue
		}
		rc, err := app.OpenRepo(wt, a.Clock)
		if err != nil {
			// A checkout that cannot be opened is left out; it is covered again once it can be.
			gone = append(gone, wt)
			continue
		}
		opened = append(opened, rc)
		covered = append(covered, coveredRepo{rc, false, "", markUnserved})
	}
	return covered, gone, func() {
		for _, rc := range opened {
			_ = rc.Close()
		}
	}
}

// recordRosterSession records conversation id as a session in rc when rc has
// none yet, started at startedAt, with no end.
func recordRosterSession(ctx context.Context, rc *app.RepoContext, id, agent string, startedAt int64) error {
	if ok, err := hasSession(ctx, rc.Store, id); err != nil || ok {
		return err
	}
	return rc.Store.WriteTx(ctx, func(c store.Conn) error {
		_, err := insertSession(ctx, c, id, agent, rc.Git, startedAt, sql.NullInt64{})
		return err
	})
}

// ensureRosterSessions records ev's conversation in each covered repo that has
// no session of it yet, side by side. Its writes give up at ctx's write
// deadline; a repo whose writer is busy gets its session from the background
// warm this start launches, or from a later start or Stop. It returns every
// other failure.
func ensureRosterSessions(ctx context.Context, covered []coveredRepo, ev Event, conv conversationStart) error {
	errs := make([]error, len(covered))
	var wg sync.WaitGroup
	for i, c := range covered {
		wg.Go(func() {
			defer recoverHook()
			err := recordRosterSession(ctx, c.RepoContext, ev.SessionID, conv.agent, conv.startedAt)
			// A busy writer is not a failure: the folder row and roster already
			// record the session, and the background warm retries until the row is written.
			if err != nil && !store.IsBusy(err) {
				errs[i] = fmt.Errorf("record the session in %s: %w", c.Git.Worktree, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// rosterRetryWait waits before RecordRosterSessions tries a busy repo again.
// A var so a test can step the retries.
var rosterRetryWait = func(ctx context.Context) error {
	t := time.NewTimer(time.Second)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RecordRosterSessions records sessionID's conversation in each roster repo it
// can open that has no session of it yet, trying a busy repo again until its
// row is written. A conversation without a roster costs one read. recarto warm
// runs it.
func RecordRosterSessions(ctx context.Context, a *app.App, sessionID string) error {
	a, closeConv, err := conversationApp(ctx, a, sessionID)
	if err != nil {
		return err
	}
	defer closeConv()
	roster, err := readRoster(ctx, a.Ledger, sessionID)
	if err != nil || len(roster) == 0 {
		return err
	}
	startedAt, agent, found, err := folderSession(ctx, a.Store, sessionID)
	if err != nil || !found {
		return err
	}
	// A repo that cannot be opened keeps its row missing: a later start or Stop
	// that can open it records it.
	covered, _, done := cover(a, roster)
	defer done()
	errs := make([]error, len(covered))
	var wg sync.WaitGroup
	for i, c := range covered {
		rc := c.RepoContext
		wg.Go(func() {
			for {
				err := recordRosterSession(store.WithWriteDeadline(ctx, time.Now().Add(writeGrace)), rc, sessionID, agent, startedAt)
				if !store.IsBusy(err) {
					errs[i] = err
					return
				}
				if err := rosterRetryWait(ctx); err != nil {
					errs[i] = err
					return
				}
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// endRosterSessions ends the session ev's conversation opened in each roster
// repo it can open, recording it first when the repo has none. A repo that
// cannot be opened keeps its open session: a later Stop that can open it ends it.
func endRosterSessions(ctx context.Context, a *app.App, ev Event, now int64) error {
	roster, err := readRoster(ctx, a.Ledger, ev.SessionID)
	if err != nil || len(roster) == 0 {
		return err
	}
	startedAt, agent, found, err := folderSession(ctx, a.Store, ev.SessionID)
	if err != nil || !found {
		return err
	}
	// A repo that cannot be opened keeps its session open: a later Stop that
	// can open it ends it.
	covered, _, done := cover(a, roster)
	defer done()
	var errs []error
	for _, c := range covered {
		rc := c.RepoContext
		var ended bool
		err := rc.Store.DB.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM sessions WHERE id=? AND ended_at IS NOT NULL)`, ev.SessionID).Scan(&ended)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ended {
			continue
		}
		// One statement closes the row whichever process wrote it, and never
		// overwrites an end already written.
		errs = append(errs, rc.Store.WriteTx(ctx, func(c store.Conn) error {
			_, err := c.ExecContext(ctx,
				`INSERT INTO sessions(id, agent, branch, worktree, started_at, ended_at)
				 VALUES (?, ?, ?, ?, ?, ?)
				 ON CONFLICT(id) DO UPDATE SET ended_at = excluded.ended_at WHERE sessions.ended_at IS NULL`,
				ev.SessionID, agent, nullable(rc.Git.Branch), rc.Git.Worktree, startedAt, now)
			return err
		}))
	}
	return errors.Join(errs...)
}

// goneBlock is the block of a conversation none of whose roster repos can be
// opened: the workspace tier, and a line naming each gone checkout.
func goneBlock(ctx, skip context.Context, a *app.App, scope string, resumed bool, gone []string) (Result, error) {
	sessionStartUpkeep(skip, a, nil)
	budget := sessionBudget()
	var wsResults []memory.RecallResult
	wsName := ""
	if a.Workspace != nil {
		wsName = a.Workspace.Name
		if a.Features.On(features.SessionMemories) {
			if set, err := a.Workspace.Engine.Recall(skip, memory.RecallQuery{Baseline: true, TokenBudget: budget / 3}); err == nil {
				wsResults = set.Results
			}
		}
	}
	if !a.Features.On(features.Todos) {
		wsResults = slices.DeleteFunc(wsResults, func(r memory.RecallResult) bool { return r.Record.Kind == memory.KindTodo })
	}
	if resumed {
		return resumeContext(ctx, a, scope, nil, memoryHeader, wsName, wsResults, budget-budget/3)
	}
	var b strings.Builder
	ws, emitted := formatWorkspaceMemories(wsName, wsResults)
	b.WriteString(ws)
	for _, wt := range gone {
		fmt.Fprintf(&b, "\n%s: unavailable; its checkout cannot be opened.\n", wt)
	}
	if f := toolFeatures(a); f.On(features.SessionMemories) && f.On(features.MemoryTools) {
		b.WriteString("\nRecord new learnings with the recarto MCP tool `record_memory` and bug fixes with `record_bug`.")
	}
	markSurfaced(ctx, a, scope, emitted)
	if scope != "" {
		_ = a.Ledger.Incr(ctx, scope, blockMetric, 1)
	}
	return Result{Context: strings.TrimSpace(b.String())}, nil
}
