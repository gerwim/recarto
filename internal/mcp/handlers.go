// Package mcp exposes the Recarto memory engine as Model Context Protocol tools.
// The tool logic in this file is deliberately SDK-free: each handler takes an
// *app.App and a plain input struct and returns a plain output struct, so it is
// unit-testable without any transport. The official MCP Go SDK is used only in
// server.go, behind the addTool adapter — so SDK churn touches one file (mirrors
// the swappable Lexical boundary in internal/memory).
package mcp

import (
	"context"
	"fmt"
	"sort"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/features"
	"github.com/gerwim/recarto/internal/memory"
)

// MemoryView is the wire shape for a memory returned by recall/list/get/search.
type MemoryView struct {
	ID                 string   `json:"id"`
	Kind               string   `json:"kind"`
	Title              string   `json:"title"`
	Body               string   `json:"body"`
	Branch             string   `json:"branch,omitempty"`
	Pinned             bool     `json:"pinned,omitempty"`
	ReinforcementCount int      `json:"reinforcement_count,omitempty"`
	Score              float64  `json:"score,omitempty"`
	Caveat             string   `json:"caveat,omitempty"`
	MatchedOn          []string `json:"matched_on,omitempty"`
	// Scope is "workspace" for a memory from the cross-repo workspace store;
	// empty (omitted) for repo memories.
	Scope string `json:"scope,omitempty"`
	// Repo names the served repo (RECARTO_REPOS) a memory came from; empty
	// (omitted) for the primary repo's own memories.
	Repo string `json:"repo,omitempty"`
}

// scopeWorkspace / scopeRepo are the wire values of the record_memory scope
// field (and MemoryView.Scope, which only ever carries the workspace one).
const (
	scopeWorkspace = "workspace"
	scopeRepo      = "repo"
)

// RecordMemoryInput are the arguments to the record_memory tool. Fields without
// `omitempty` (kind, body) are required in the generated JSON schema.
type RecordMemoryInput struct {
	Kind         string   `json:"kind" jsonschema:"preference, learning, decision, do_not_repeat, bug or todo (open work that resurfaces until done)"`
	Title        string   `json:"title,omitempty" jsonschema:"short one-line summary"`
	Body         string   `json:"body" jsonschema:"full memory content"`
	Files        []string `json:"files,omitempty" jsonschema:"related file paths"`
	Symbols      []string `json:"symbols,omitempty" jsonschema:"related symbol names"`
	Tags         []string `json:"tags,omitempty" jsonschema:"freeform tags"`
	Pinned       bool     `json:"pinned,omitempty" jsonschema:"pin so it is never evicted"`
	Global       bool     `json:"global,omitempty" jsonschema:"store repo-global instead of branch-scoped"`
	Scope        string   `json:"scope,omitempty" jsonschema:"where to store it: repo (default) or workspace"`
	Repo         string   `json:"repo,omitempty" jsonschema:"store into this served repo (name or path) instead of the current one; not with scope=workspace"`
	SupersedesID string   `json:"supersedes_id,omitempty" jsonschema:"id of a prior memory this replaces"`
}

// ConflictCandidateOut is one existing memory the new one may contradict.
type ConflictCandidateOut struct {
	ID      string `json:"id"`
	Title   string `json:"title,omitempty"`
	Snippet string `json:"snippet,omitempty"`
	Reason  string `json:"reason"` // similar wording | shares refs
}

// RecordMemoryOutput is the result of the record_memory tool.
type RecordMemoryOutput struct {
	ID         string `json:"id" jsonschema:"id of the recorded or reinforced memory"`
	Reinforced bool   `json:"reinforced" jsonschema:"true if an existing near-duplicate was reinforced"`
	// ConflictCandidates are existing memories the new one may contradict or
	// supersede — similar topic, different claim. Advisory: the write already
	// happened; review and resolve via Note's instructions.
	ConflictCandidates []ConflictCandidateOut `json:"conflict_candidates,omitempty"`
	Note               string                 `json:"note,omitempty"`
}

func recordMemory(ctx context.Context, a *app.App, in RecordMemoryInput) (RecordMemoryOutput, error) {
	kind := memory.Kind(in.Kind)
	if !kind.Valid() {
		return RecordMemoryOutput{}, fmt.Errorf("invalid kind %q", in.Kind)
	}
	if kind == memory.KindFeedback {
		return RecordMemoryOutput{}, fmt.Errorf("kind %q is recorded via report_recarto_issue, not record_memory", in.Kind)
	}
	if kind == memory.KindTodo && !a.Features.On(features.Todos) {
		return RecordMemoryOutput{}, fmt.Errorf("todos are off in the Recarto config (recarto config)")
	}
	if in.Body == "" {
		return RecordMemoryOutput{}, fmt.Errorf("body is required")
	}
	eng := a.Engine
	branch := a.Git.Branch
	worktree := a.Git.Worktree
	refWorktree := a.Git.Worktree
	if in.Repo != "" {
		if in.Scope == scopeWorkspace {
			return RecordMemoryOutput{}, fmt.Errorf("repo and scope=workspace are mutually exclusive")
		}
		// Route the write to a served repo's store (RECARTO_REPOS) — e.g. a
		// repo-less verifier recording a decision about one of the task's repos.
		t, err := repoTarget(a, in.Repo)
		if err != nil {
			return RecordMemoryOutput{}, err
		}
		eng, branch, worktree, refWorktree = t.Engine, t.Git.Branch, t.Git.Worktree, t.Git.Worktree
	}
	switch in.Scope {
	case "", scopeRepo:
	case scopeWorkspace:
		if a.Workspace == nil {
			return RecordMemoryOutput{}, fmt.Errorf("scope %q requires a configured workspace (set %s)", in.Scope, "RECARTO_WORKSPACE")
		}
		// Workspace memories are branch-less and worktree-less by construction:
		// branches and worktrees are repo concepts. File refs still normalize
		// against the current worktree so they read repo-relative.
		eng, branch, worktree = a.Workspace.Engine, "", ""
	default:
		return RecordMemoryOutput{}, fmt.Errorf("invalid scope %q (repo or workspace)", in.Scope)
	}
	if in.Global {
		branch = ""
	}
	id, reinforced, conflicts, err := eng.Record(ctx, memory.NewMemory{
		Kind:         kind,
		Title:        in.Title,
		Body:         in.Body,
		Refs:         refsFrom(refWorktree, in.Files, in.Symbols, in.Tags),
		Branch:       branch,
		Worktree:     worktree,
		Author:       "mcp",
		Source:       "mcp",
		Pinned:       in.Pinned,
		SupersedesID: in.SupersedesID,
	})
	if err != nil {
		return RecordMemoryOutput{}, err
	}
	a.SignalSync()
	out := RecordMemoryOutput{ID: id, Reinforced: reinforced}
	for _, c := range conflicts {
		out.ConflictCandidates = append(out.ConflictCandidates, ConflictCandidateOut{
			ID: c.ID, Title: c.Title, Snippet: c.Snippet, Reason: c.Reason,
		})
	}
	if len(out.ConflictCandidates) > 0 {
		out.Note = "Review these existing memories — if your new memory supersedes or contradicts one, call forget_memory(old_id) (or re-record with supersedes_id). If they genuinely coexist, no action needed."
	}
	return out, nil
}

// RecallMemoryInput are the arguments to the recall_memory tool (all optional).
type RecallMemoryInput struct {
	TaskHint    string   `json:"task_hint,omitempty" jsonschema:"free-text description of the current task"`
	Files       []string `json:"files,omitempty" jsonschema:"files relevant to the current task"`
	Symbols     []string `json:"symbols,omitempty" jsonschema:"symbols relevant to the current task"`
	Kind        string   `json:"kind,omitempty" jsonschema:"restrict to one kind; pinned memories are filtered too"`
	Limit       int      `json:"limit,omitempty" jsonschema:"maximum memories to return"`
	TokenBudget int      `json:"token_budget,omitempty" jsonschema:"token budget for the returned set"`
	MinScore    float64  `json:"min_score,omitempty" jsonschema:"drop candidates scoring below this (0-1)"`
}

// RecallMemoryOutput is the result of the recall_memory tool.
type RecallMemoryOutput struct {
	Count      int          `json:"count"`
	Memories   []MemoryView `json:"memories"`
	Truncated  bool         `json:"truncated"`
	BudgetUsed int          `json:"budget_used"`
}

// scopedResult pairs a recall result with the store it came from: scope is
// scopeWorkspace for the workspace store, repo names a RECARTO_REPOS extra;
// both empty = the primary repo store.
type scopedResult struct {
	memory.RecallResult
	scope string
	repo  string
}

// recallScoped runs q against the primary repo engine, every served extra
// repo's engine (RECARTO_REPOS — each with its own branch/worktree), and —
// when a workspace is configured — the workspace engine (branch/worktree
// stripped: workspace rows carry neither), returning the merged results
// sorted by score. Scores from every store are computed by the identical
// algorithm, so a merged sort is meaningful. Each store applies q's
// limit/budget independently; callers that need a hard cap re-cap the merged
// slice. Extra-store and workspace failures are skipped, never fatal: partial
// recall beats none.
func recallScoped(ctx context.Context, a *app.App, q memory.RecallQuery) ([]scopedResult, int, bool, error) {
	set, err := a.Engine.Recall(ctx, q)
	if err != nil {
		return nil, 0, false, err
	}
	merged := make([]scopedResult, 0, len(set.Results))
	for _, r := range set.Results {
		merged = append(merged, scopedResult{RecallResult: r})
	}
	used, truncated := set.TokensUsed, set.Truncated
	for _, rc := range a.Repos {
		rq := q
		rq.Branch, rq.Worktree = rc.Git.Branch, rc.Git.Worktree
		if rset, err := rc.Engine.Recall(ctx, rq); err == nil {
			for _, r := range rset.Results {
				merged = append(merged, scopedResult{RecallResult: r, repo: rc.Git.RepoName})
			}
			used += rset.TokensUsed
			truncated = truncated || rset.Truncated
		}
	}
	if a.Workspace != nil {
		wq := q
		wq.Branch, wq.Worktree = "", ""
		if wset, err := a.Workspace.Engine.Recall(ctx, wq); err == nil {
			for _, r := range wset.Results {
				merged = append(merged, scopedResult{RecallResult: r, scope: scopeWorkspace})
			}
			used += wset.TokensUsed
			truncated = truncated || wset.Truncated
		}
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Score > merged[j].Score })
	return merged, used, truncated, nil
}

func recallMemory(ctx context.Context, a *app.App, in RecallMemoryInput) (RecallMemoryOutput, error) {
	merged, used, truncated, err := recallScoped(ctx, a, memory.RecallQuery{
		TaskHint:    in.TaskHint,
		Files:       in.Files,
		Symbols:     in.Symbols,
		Kind:        memory.Kind(in.Kind),
		Branch:      a.Git.Branch,
		Worktree:    a.Git.Worktree,
		Limit:       in.Limit,
		TokenBudget: in.TokenBudget,
		MinScore:    in.MinScore,
	})
	if err != nil {
		return RecallMemoryOutput{}, err
	}
	if in.Limit > 0 && len(merged) > in.Limit {
		merged = merged[:in.Limit]
	}
	out := RecallMemoryOutput{
		Count: len(merged), Memories: make([]MemoryView, 0, len(merged)),
		Truncated: truncated, BudgetUsed: used,
	}
	for _, r := range merged {
		v := viewOf(r.Record)
		v.Score = roundScore(r.Score)
		v.Caveat = r.Caveat()
		v.MatchedOn = r.MatchedOn()
		v.Scope = r.scope
		v.Repo = r.repo
		out.Memories = append(out.Memories, v)
	}
	return out, nil
}

// refsFrom builds structural refs from file/symbol/tag value lists. File values
// are normalized to repo-relative form so they match hook-reported absolute paths.
func refsFrom(worktree string, files, symbols, tags []string) []memory.Ref {
	var refs []memory.Ref
	for _, f := range files {
		refs = append(refs, memory.Ref{Type: memory.RefFile, Value: memory.RelFileRef(worktree, f)})
	}
	for _, s := range symbols {
		refs = append(refs, memory.Ref{Type: memory.RefSymbol, Value: s})
	}
	for _, tg := range tags {
		refs = append(refs, memory.Ref{Type: memory.RefTag, Value: tg})
	}
	return refs
}

// CompleteTodoInput is the argument to the complete_todo tool.
type CompleteTodoInput struct {
	ID string `json:"id" jsonschema:"id of the todo memory to mark done"`
}

// CompleteTodoOutput reports whether an active todo was transitioned to done.
type CompleteTodoOutput struct {
	Completed bool `json:"completed"`
}

func completeTodo(ctx context.Context, a *app.App, in CompleteTodoInput) (CompleteTodoOutput, error) {
	var ok bool
	for _, rc := range a.AllRepos() {
		done, err := rc.Engine.CompleteTodo(ctx, in.ID)
		if err != nil {
			return CompleteTodoOutput{}, err
		}
		if done {
			ok = true
			break
		}
	}
	if !ok && a.Workspace != nil {
		var err error
		if ok, err = a.Workspace.Engine.CompleteTodo(ctx, in.ID); err != nil {
			return CompleteTodoOutput{}, err
		}
	}
	if ok {
		a.SignalSync()
	}
	return CompleteTodoOutput{Completed: ok}, nil
}

// viewOf projects a stored Record onto the wire shape (Score is set by callers).
func viewOf(r *memory.Record) MemoryView {
	return MemoryView{
		ID:                 r.ID,
		Kind:               string(r.Kind),
		Title:              r.Title,
		Body:               r.Body,
		Branch:             r.Branch,
		Pinned:             r.Pinned,
		ReinforcementCount: r.ReinforcementCount,
	}
}

// RecordBugInput are the arguments to the record_bug convenience tool.
type RecordBugInput struct {
	ErrorMessage string   `json:"error_message" jsonschema:"the error text or user complaint"`
	RootCause    string   `json:"root_cause,omitempty" jsonschema:"why it broke"`
	Fix          string   `json:"fix,omitempty" jsonschema:"what fixed it"`
	File         string   `json:"file,omitempty" jsonschema:"the file that was fixed"`
	Symbols      []string `json:"symbols,omitempty" jsonschema:"related symbol names"`
	Tags         []string `json:"tags,omitempty" jsonschema:"freeform tags"`
	Repo         string   `json:"repo,omitempty" jsonschema:"record into this served repo (name or path) instead of the current one"`
}

// RecordBugOutput is the result of the record_bug tool.
type RecordBugOutput struct {
	ID         string `json:"id"`
	Reinforced bool   `json:"reinforced"`
}

func recordBug(ctx context.Context, a *app.App, in RecordBugInput) (RecordBugOutput, error) {
	if in.ErrorMessage == "" {
		return RecordBugOutput{}, fmt.Errorf("error_message is required")
	}
	t, err := repoTarget(a, in.Repo)
	if err != nil {
		return RecordBugOutput{}, err
	}
	extra := map[string]any{
		"error_message": in.ErrorMessage,
		"root_cause":    in.RootCause,
		"fix":           in.Fix,
		"file":          in.File,
	}
	if len(in.Tags) > 0 {
		extra["tags"] = in.Tags
	}
	var refs []memory.Ref
	if in.File != "" {
		refs = append(refs, memory.Ref{Type: memory.RefFile, Value: memory.RelFileRef(t.Git.Worktree, in.File)})
	}
	for _, s := range in.Symbols {
		refs = append(refs, memory.Ref{Type: memory.RefSymbol, Value: s})
	}
	// The body carries error + fix so the lexical index can match either.
	body := in.ErrorMessage
	if in.Fix != "" {
		body = in.ErrorMessage + " — fix: " + in.Fix
	}
	id, reinforced, _, err := t.Engine.Record(ctx, memory.NewMemory{
		Kind:     memory.KindBug,
		Title:    in.ErrorMessage,
		Body:     body,
		Refs:     refs,
		Branch:   t.Git.Branch,
		Worktree: t.Git.Worktree,
		Author:   "mcp",
		Source:   "mcp",
		Extra:    extra,
	})
	if err != nil {
		return RecordBugOutput{}, err
	}
	a.SignalSync()
	return RecordBugOutput{ID: id, Reinforced: reinforced}, nil
}

// SearchBugsInput is the argument to the search_bugs tool.
type SearchBugsInput struct {
	Query string `json:"query" jsonschema:"keywords or a plain-words description of the bug"`
}

// SearchBugsOutput is the result of the search_bugs tool.
type SearchBugsOutput struct {
	Count int          `json:"count"`
	Bugs  []MemoryView `json:"bugs"`
}

func searchBugs(ctx context.Context, a *app.App, in SearchBugsInput) (SearchBugsOutput, error) {
	merged, _, _, err := recallScoped(ctx, a, memory.RecallQuery{
		TaskHint: in.Query,
		Kind:     memory.KindBug,
		Branch:   a.Git.Branch,
		Worktree: a.Git.Worktree,
	})
	if err != nil {
		return SearchBugsOutput{}, err
	}
	out := SearchBugsOutput{Count: len(merged), Bugs: make([]MemoryView, 0, len(merged))}
	for _, r := range merged {
		v := viewOf(r.Record)
		v.Score = roundScore(r.Score)
		v.Caveat = r.Caveat()
		v.MatchedOn = r.MatchedOn()
		v.Scope = r.scope
		v.Repo = r.repo
		out.Bugs = append(out.Bugs, v)
	}
	return out, nil
}

// GetMemoryInput is the argument to the get_memory tool.
type GetMemoryInput struct {
	ID string `json:"id" jsonschema:"the memory id"`
}

// GetMemoryOutput is the result of the get_memory tool.
type GetMemoryOutput struct {
	Found  bool        `json:"found"`
	Memory *MemoryView `json:"memory,omitempty"`
}

func getMemory(ctx context.Context, a *app.App, in GetMemoryInput) (GetMemoryOutput, error) {
	// Ids are unique across stores (UUIDv7), so an id the primary store does
	// not know may live in a served repo's store or the workspace store —
	// every id-taking tool checks them all.
	for _, rc := range a.AllRepos() {
		rec, err := rc.Engine.GetByID(ctx, in.ID)
		if err != nil {
			return GetMemoryOutput{}, err
		}
		if rec != nil {
			v := viewOf(rec)
			if rc.Git.RepoID != a.Git.RepoID {
				v.Repo = rc.Git.RepoName
			}
			return GetMemoryOutput{Found: true, Memory: &v}, nil
		}
	}
	if a.Workspace != nil {
		rec, err := a.Workspace.Engine.GetByID(ctx, in.ID)
		if err != nil {
			return GetMemoryOutput{}, err
		}
		if rec != nil {
			v := viewOf(rec)
			v.Scope = scopeWorkspace
			return GetMemoryOutput{Found: true, Memory: &v}, nil
		}
	}
	return GetMemoryOutput{Found: false}, nil
}

// ListMemoryInput are the arguments to the list_memory tool (all optional).
type ListMemoryInput struct {
	Kind   string `json:"kind,omitempty" jsonschema:"restrict to a single kind"`
	Branch string `json:"branch,omitempty" jsonschema:"restrict to a branch (default: all branches)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max rows to return"`
}

// ListMemoryOutput is the result of the list_memory tool.
type ListMemoryOutput struct {
	Count    int          `json:"count"`
	Memories []MemoryView `json:"memories"`
}

func listMemory(ctx context.Context, a *app.App, in ListMemoryInput) (ListMemoryOutput, error) {
	f := memory.ListFilter{
		Kind:   memory.Kind(in.Kind),
		Branch: in.Branch,
		Limit:  in.Limit,
	}
	recs, err := a.Engine.List(ctx, f)
	if err != nil {
		return ListMemoryOutput{}, err
	}
	views := make([]MemoryView, 0, len(recs))
	at := make(map[string]int64, len(recs))
	for _, r := range recs {
		views = append(views, viewOf(r))
		at[r.ID] = r.UpdatedAt
	}
	merged := false
	for _, rc := range a.Repos {
		// A served repo's rows are listed against its OWN branch filter only
		// when the caller asked for one; the label tells them apart.
		if rrecs, err := rc.Engine.List(ctx, f); err == nil {
			merged = merged || len(rrecs) > 0
			for _, r := range rrecs {
				v := viewOf(r)
				v.Repo = rc.Git.RepoName
				views = append(views, v)
				at[r.ID] = r.UpdatedAt
			}
		}
	}
	if a.Workspace != nil {
		// Workspace rows are branch-less, so the branch filter never applies.
		wf := f
		wf.Branch = ""
		if wrecs, err := a.Workspace.Engine.List(ctx, wf); err == nil {
			merged = merged || len(wrecs) > 0
			for _, r := range wrecs {
				v := viewOf(r)
				v.Scope = scopeWorkspace
				views = append(views, v)
				at[r.ID] = r.UpdatedAt
			}
		}
	}
	if merged {
		sort.SliceStable(views, func(i, j int) bool { return at[views[i].ID] > at[views[j].ID] })
		if in.Limit > 0 && len(views) > in.Limit {
			views = views[:in.Limit]
		}
	}
	return ListMemoryOutput{Count: len(views), Memories: views}, nil
}

// ForgetMemoryInput is the argument to the forget_memory tool.
type ForgetMemoryInput struct {
	ID     string `json:"id" jsonschema:"id of the memory to delete"`
	Reason string `json:"reason,omitempty" jsonschema:"why it is being forgotten (advisory; not stored)"`
}

// ForgetMemoryOutput reports whether a memory was deleted.
type ForgetMemoryOutput struct {
	Forgotten bool `json:"forgotten"`
}

func forgetMemory(ctx context.Context, a *app.App, in ForgetMemoryInput) (ForgetMemoryOutput, error) {
	var ok bool
	for _, rc := range a.AllRepos() {
		gone, err := rc.Engine.Forget(ctx, in.ID, in.Reason)
		if err != nil {
			return ForgetMemoryOutput{}, err
		}
		if gone {
			ok = true
			break
		}
	}
	if !ok && a.Workspace != nil {
		var err error
		if ok, err = a.Workspace.Engine.Forget(ctx, in.ID, in.Reason); err != nil {
			return ForgetMemoryOutput{}, err
		}
	}
	if ok {
		a.SignalSync()
	}
	return ForgetMemoryOutput{Forgotten: ok}, nil
}
