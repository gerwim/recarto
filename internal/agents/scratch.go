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
	"github.com/gerwim/recarto/internal/ledger"
	"github.com/gerwim/recarto/internal/memory"
	"github.com/gerwim/recarto/internal/store"
)

// rosterMetric prefixes a conversation's roster entries: one per repo it
// covers, valued by its 1-based position.
const rosterMetric = "roster:"

const (
	rosterRepoMetric = "roster-repo:" // per conversation: "roster-repo:<id>:<root>:<wt>" = position
	folderReposScope = "roster-repos" // per folder store: one metric per repository id any conversation of the folder had
)

// rosterRepo is one repo of a conversation's roster: its checkout path, its
// repository's cache id and a root commit of it ("" when unknown).
type rosterRepo struct{ wt, id, root string }

// sameRepository reports whether the checkout rc is the repository with root
// commit root: root unknown, or a commit rc's repository holds now. It asks git
// (gitctx.HasCommit), because the identity cache's RootCommit can predate a
// checkout replaced in place.
func sameRepository(rc *app.RepoContext, root string) bool {
	return root == "" || gitctx.HasCommit(gitctx.Real{Dir: rc.Git.Worktree}, root)
}

// rosterOf returns the roster entries of repos, in order, each with a root commit
// its checkout holds now.
func rosterOf(repos []*app.RepoContext) []rosterRepo {
	out := make([]rosterRepo, len(repos))
	var wg sync.WaitGroup
	for i, rc := range repos {
		wg.Go(func() {
			r := gitctx.Real{Dir: rc.Git.Worktree}
			root := rc.Git.RootCommit
			if !gitctx.HasCommit(r, root) {
				// The cached identity predates a checkout replaced in place.
				root = gitctx.RootCommit(r)
			}
			out[i] = rosterRepo{rc.Git.Worktree, rc.Git.RepoID, root}
		})
	}
	wg.Wait()
	return out
}

// rosterScope is the ledger scope a conversation's roster lives in: its main
// agent's, whose roster entries a compact or clear does not forget.
func rosterScope(sessionID string) string { return "session:" + sessionID }

// writeRoster records roster as sessionID's roster, in order, and each of its
// repositories as one the folder's conversations had.
func writeRoster(ctx context.Context, c store.Conn, sessionID string, roster []rosterRepo) error {
	put := func(scope, metric string, value int) error {
		_, err := c.ExecContext(ctx,
			`INSERT INTO counters(scope, metric, value) VALUES (?, ?, ?)
			 ON CONFLICT(scope, metric) DO NOTHING`, scope, metric, value)
		return err
	}
	for i, r := range roster {
		if err := errors.Join(
			put(rosterScope(sessionID), rosterMetric+r.wt, i+1),
			put(rosterScope(sessionID), rosterRepoMetric+r.id+":"+r.root+":"+r.wt, i+1),
			put(folderReposScope, r.id, 1)); err != nil {
			return err
		}
	}
	return nil
}

// readRoster returns sessionID's roster in order, nil when it has none. An entry
// recorded without its repository has an empty id and root.
func readRoster(ctx context.Context, l *ledger.Ledger, sessionID string) ([]rosterRepo, error) {
	metrics, err := l.Metrics(ctx, rosterScope(sessionID), rosterMetric)
	if err != nil {
		return nil, err
	}
	repos, err := l.Metrics(ctx, rosterScope(sessionID), rosterRepoMetric)
	if err != nil {
		return nil, err
	}
	type entry struct {
		rosterRepo
		pos int64
	}
	entries := make([]entry, 0, len(metrics))
	for _, m := range metrics {
		pos, err := l.Get(ctx, rosterScope(sessionID), m)
		if err != nil {
			return nil, err
		}
		e := entry{rosterRepo{wt: strings.TrimPrefix(m, rosterMetric)}, pos}
		for _, r := range repos {
			// The worktree comes last, so a ':' in its path stays in it.
			if f := strings.SplitN(strings.TrimPrefix(r, rosterRepoMetric), ":", 3); len(f) == 3 && f[2] == e.wt {
				e.id, e.root = f[0], f[1]
				break
			}
		}
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(x, y entry) int { return int(x.pos - y.pos) })
	var out []rosterRepo
	for _, e := range entries {
		out = append(out, e.rosterRepo)
	}
	return out, nil
}

// folderRepoIDs returns the ids of the repositories any conversation of the
// folder whose ledger l is had.
func folderRepoIDs(ctx context.Context, l *ledger.Ledger) ([]string, error) {
	return l.Metrics(ctx, folderReposScope, "")
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

type folderRow struct {
	startedAt int64
	agent     string
	ended     sql.NullInt64
}

// folderSession reads the start, agent and end of sessionID's row in st.
func folderSession(ctx context.Context, st *store.Store, sessionID string) (row folderRow, found bool, err error) {
	err = st.DB.QueryRowContext(ctx,
		`SELECT started_at, agent, ended_at FROM sessions WHERE id=?`, sessionID).Scan(&row.startedAt, &row.agent, &row.ended)
	if errors.Is(err, sql.ErrNoRows) {
		return folderRow{}, false, nil
	}
	return row, err == nil, err
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
	var roster []rosterRepo
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
	roster    []rosterRepo // the repos it covers, in order; nil: the block of the session's own worktree
	first     bool         // this start is the first Recarto sees of the conversation
	startedAt int64        // the folder session's start
	agent     string       // the folder session's agent, for its roster repo sessions
}

// startConversation records ev's conversation at its first session start and
// returns what the start knows of it. The roster is written with the
// conversation's row, before any repo gets a session of it, so no repo outside
// the saved roster ever gets one.
func startConversation(ctx context.Context, a *app.App, ev Event, now int64, wait *startWait) (conversationStart, error) {
	if ev.SessionID == "" {
		conv := conversationStart{first: true, startedAt: now, agent: agentOrDefault(ev.Agent)}
		if codeless(a, wait) {
			conv.roster = rosterOf(a.Repos)
		}
		return conv, nil
	}
	known := func() (conversationStart, error) {
		row, _, err := folderSession(ctx, a.Store, ev.SessionID)
		if err != nil {
			return conversationStart{}, err
		}
		roster, err := readRoster(ctx, a.Ledger, ev.SessionID)
		return conversationStart{roster: roster, startedAt: row.startedAt, agent: row.agent}, err
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
	var roster []rosterRepo
	if codeless(a, wait) {
		roster = rosterOf(a.Repos)
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
	return app.CodeLess(wait.get(), a.Git)
}

// coveredRepo is a roster repo a block covers.
type coveredRepo struct {
	*app.RepoContext
	served bool   // RECARTO_REPOS serves it now: it gets the first-index wait
	sel    string // its selector; "" when the repo argument cannot reach it
	mark   string // "" with a selector, else why it has none
	root   string // the roster's root commit for it
	roster func() bool
}

// isRoster reports whether the open checkout is still the roster's repository.
// It asks git once, and only where a write depends on it.
func (c coveredRepo) isRoster() bool { return c.roster() }

// Marks of a covered repo the repo argument cannot reach.
const (
	markUnserved = "no longer served"
)

const markUnselectable = app.Unselectable

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
func cover(a *app.App, roster []rosterRepo) (covered []coveredRepo, gone []string, done func()) {
	var opened []*app.RepoContext
	for _, e := range roster {
		c := coveredRepo{root: e.root}
		if i := slices.IndexFunc(a.Repos, func(rc *app.RepoContext) bool { return rc.Git.Worktree == e.wt }); i >= 0 {
			rc := a.Repos[i]
			c.RepoContext, c.served = rc, true
			if sel, ok := a.Selector(rc); ok {
				c.sel = sel
			} else {
				c.mark = markUnselectable
			}
		} else {
			rc, err := app.OpenRepo(e.wt, a.Clock)
			if err != nil {
				// A checkout that cannot be opened is left out; it is covered again once it can be.
				gone = append(gone, e.wt)
				continue
			}
			opened = append(opened, rc)
			c.RepoContext, c.mark = rc, markUnserved
		}
		rc := c.RepoContext
		c.roster = sync.OnceValue(func() bool {
			return e.id == "" || (e.id == rc.Git.RepoID && sameRepository(rc, e.root))
		})
		covered = append(covered, c)
	}
	return covered, gone, func() {
		for _, rc := range opened {
			_ = rc.Close()
		}
	}
}

// recordRosterSession records conversation id as a session in c when c has
// none yet and is still the roster's repository, started at startedAt, with no end.
func recordRosterSession(ctx context.Context, c coveredRepo, id, agent string, startedAt int64) error {
	rc := c.RepoContext
	if ok, err := hasSession(ctx, rc.Store, id); err != nil || ok || !c.isRoster() {
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
			err := recordRosterSession(ctx, c, ev.SessionID, conv.agent, conv.startedAt)
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
	row, found, err := folderSession(ctx, a.Store, sessionID)
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
		wg.Go(func() {
			for {
				err := recordRosterSession(store.WithWriteDeadline(ctx, time.Now().Add(writeGrace)), c, sessionID, row.agent, row.startedAt)
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
