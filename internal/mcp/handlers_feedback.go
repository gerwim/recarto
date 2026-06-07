package mcp

import (
	"context"
	"fmt"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/feedback"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/version"
)

// ReportRecartoIssueInput are the arguments to report_recarto_issue. signal_type,
// title and detail are required; the rest are optional. Context (source repo,
// version) is auto-captured by the handler — the agent does not supply it.
type ReportRecartoIssueInput struct {
	SignalType   string `json:"signal_type" jsonschema:"the kind of problem: error (a tool errored/crashed/returned a clearly-wrong empty result), quality (it worked but ranked/returned the wrong thing), friction (confusing or awkward API), or gap (a capability you needed was missing)"`
	Title        string `json:"title" jsonschema:"one-line summary of the problem"`
	Detail       string `json:"detail" jsonschema:"what you were doing, what you expected, and what actually happened. Describe the Recarto interaction and a minimal repro — do NOT paste proprietary source from this repo"`
	Tool         string `json:"tool,omitempty" jsonschema:"which Recarto tool or area was involved (e.g. search_content, trace_call_path)"`
	Severity     string `json:"severity,omitempty" jsonschema:"low, medium, or high"`
	Repro        string `json:"repro,omitempty" jsonschema:"the call/args that triggered it"`
	SuggestedFix string `json:"suggested_fix,omitempty" jsonschema:"your proposed fix, if any"`
}

// ReportRecartoIssueOutput is the result of report_recarto_issue.
type ReportRecartoIssueOutput struct {
	ID         string `json:"id" jsonschema:"id of the recorded or reinforced report"`
	Reinforced bool   `json:"reinforced" jsonschema:"true if a near-duplicate report (e.g. from another repo) was reinforced instead of duplicated"`
}

// reportRecartoIssue records a report about Recarto itself into the dedicated
// global feedback store (NOT the current repo's store). The source repo is carried
// as a tag ref so that a report seen in several repos reinforces one entry while
// accumulating the set of repos it was seen in.
func reportRecartoIssue(ctx context.Context, a *app.App, in ReportRecartoIssueInput) (ReportRecartoIssueOutput, error) {
	if in.SignalType == "" || in.Title == "" || in.Detail == "" {
		return ReportRecartoIssueOutput{}, fmt.Errorf("signal_type, title and detail are required")
	}
	b, err := feedback.Open(a.Clock)
	if err != nil {
		return ReportRecartoIssueOutput{}, err
	}
	defer b.Close()

	source := a.Git.RepoName
	if source == "" {
		source = a.Git.RepoID
	}
	extra := map[string]any{
		"signal_type":      in.SignalType,
		"severity":         in.Severity,
		"tool":             in.Tool,
		"repro":            in.Repro,
		"suggested_fix":    in.SuggestedFix,
		"source_repo_id":   a.Git.RepoID,
		"source_repo_name": a.Git.RepoName,
		"recarto_version":  version.Version,
	}
	// Source repo as a tag ref: reinforce unions refs, so one entry accumulates every
	// repo it was reported from (occurrence count = reinforcement_count + 1).
	var refs []memory.Ref
	if source != "" {
		refs = append(refs, memory.Ref{Type: memory.RefTag, Value: feedback.RepoRefPrefix + source})
	}
	id, reinforced, err := b.Engine.Record(ctx, memory.NewMemory{
		Kind:   memory.KindFeedback,
		Title:  in.Title,
		Body:   in.Title + "\n\n" + in.Detail, // body drives dedup + lexical match
		Refs:   refs,
		Branch: "", // global — feedback is not branch-scoped
		Author: "mcp",
		Source: "mcp",
		Extra:  extra,
	})
	if err != nil {
		return ReportRecartoIssueOutput{}, err
	}
	return ReportRecartoIssueOutput{ID: id, Reinforced: reinforced}, nil
}
