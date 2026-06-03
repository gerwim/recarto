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

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// MemoryView is the wire shape for a memory returned by recall/list/get/search.
type MemoryView struct {
	ID                 string  `json:"id"`
	Kind               string  `json:"kind"`
	Title              string  `json:"title"`
	Body               string  `json:"body"`
	Branch             string  `json:"branch,omitempty"`
	Pinned             bool    `json:"pinned,omitempty"`
	ReinforcementCount int     `json:"reinforcement_count,omitempty"`
	Score              float64 `json:"score,omitempty"`
}

// RecordMemoryInput are the arguments to the record_memory tool. Fields without
// `omitempty` (kind, body) are required in the generated JSON schema.
type RecordMemoryInput struct {
	Kind         string   `json:"kind" jsonschema:"memory kind: preference, learning, decision, do_not_repeat, bug, or action"`
	Title        string   `json:"title,omitempty" jsonschema:"short one-line summary"`
	Body         string   `json:"body" jsonschema:"full memory content"`
	Files        []string `json:"files,omitempty" jsonschema:"related file paths"`
	Symbols      []string `json:"symbols,omitempty" jsonschema:"related symbol names"`
	Tags         []string `json:"tags,omitempty" jsonschema:"freeform tags"`
	Pinned       bool     `json:"pinned,omitempty" jsonschema:"pin so it is never evicted"`
	Global       bool     `json:"global,omitempty" jsonschema:"store repo-global instead of branch-scoped"`
	SupersedesID string   `json:"supersedes_id,omitempty" jsonschema:"id of a prior memory this replaces"`
}

// RecordMemoryOutput is the result of the record_memory tool.
type RecordMemoryOutput struct {
	ID         string `json:"id" jsonschema:"id of the recorded or reinforced memory"`
	Reinforced bool   `json:"reinforced" jsonschema:"true if an existing near-duplicate was reinforced"`
}

func recordMemory(ctx context.Context, a *app.App, in RecordMemoryInput) (RecordMemoryOutput, error) {
	kind := memory.Kind(in.Kind)
	if !kind.Valid() {
		return RecordMemoryOutput{}, fmt.Errorf("invalid kind %q", in.Kind)
	}
	if in.Body == "" {
		return RecordMemoryOutput{}, fmt.Errorf("body is required")
	}
	branch := a.Git.Branch
	if in.Global {
		branch = ""
	}
	id, reinforced, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind:         kind,
		Title:        in.Title,
		Body:         in.Body,
		Refs:         refsFrom(in.Files, in.Symbols, in.Tags),
		Branch:       branch,
		Worktree:     a.Git.Worktree,
		Author:       "mcp",
		Source:       "mcp",
		Pinned:       in.Pinned,
		SupersedesID: in.SupersedesID,
	})
	if err != nil {
		return RecordMemoryOutput{}, err
	}
	return RecordMemoryOutput{ID: id, Reinforced: reinforced}, nil
}

// RecallMemoryInput are the arguments to the recall_memory tool (all optional).
type RecallMemoryInput struct {
	TaskHint    string   `json:"task_hint,omitempty" jsonschema:"free-text description of the current task"`
	Files       []string `json:"files,omitempty" jsonschema:"files relevant to the current task"`
	Symbols     []string `json:"symbols,omitempty" jsonschema:"symbols relevant to the current task"`
	Kind        string   `json:"kind,omitempty" jsonschema:"restrict to a single kind"`
	Limit       int      `json:"limit,omitempty" jsonschema:"max candidates to consider"`
	TokenBudget int      `json:"token_budget,omitempty" jsonschema:"token budget for the returned set"`
}

// RecallMemoryOutput is the result of the recall_memory tool.
type RecallMemoryOutput struct {
	Count    int          `json:"count"`
	Memories []MemoryView `json:"memories"`
}

func recallMemory(ctx context.Context, a *app.App, in RecallMemoryInput) (RecallMemoryOutput, error) {
	results, err := a.Engine.Recall(ctx, memory.RecallQuery{
		TaskHint:    in.TaskHint,
		Files:       in.Files,
		Symbols:     in.Symbols,
		Kind:        memory.Kind(in.Kind),
		Branch:      a.Git.Branch,
		Limit:       in.Limit,
		TokenBudget: in.TokenBudget,
	})
	if err != nil {
		return RecallMemoryOutput{}, err
	}
	out := RecallMemoryOutput{Count: len(results), Memories: make([]MemoryView, 0, len(results))}
	for _, r := range results {
		v := viewOf(r.Record)
		v.Score = r.Score
		out.Memories = append(out.Memories, v)
	}
	return out, nil
}

// refsFrom builds structural refs from file/symbol/tag value lists.
func refsFrom(files, symbols, tags []string) []memory.Ref {
	var refs []memory.Ref
	for _, f := range files {
		refs = append(refs, memory.Ref{Type: memory.RefFile, Value: f})
	}
	for _, s := range symbols {
		refs = append(refs, memory.Ref{Type: memory.RefSymbol, Value: s})
	}
	for _, tg := range tags {
		refs = append(refs, memory.Ref{Type: memory.RefTag, Value: tg})
	}
	return refs
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
