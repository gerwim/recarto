package agents

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/features"
	"github.com/gerwim/recarto/internal/graph"
	"github.com/gerwim/recarto/internal/ledger"
	"github.com/gerwim/recarto/internal/memory"
	"github.com/gerwim/recarto/internal/store"
)

// shareItem is one line a repo's map, bugs or changes step can take.
type shareItem struct {
	text string
	key  string // orders the step's taken items when rendered; "" keeps candidate order
	id   string // the memory a recent-bug line shows; "" for other lines
	ends []int  // a dependency's endpoint modules (indexes into steps[0].items), brought along when not yet shown
	last bool   // taken only once every earlier item of its step is taken
}

// shareStep is one fill step's candidates.
type shareStep struct {
	items []shareItem // candidates, in the step's order
	cap   int         // most items taken over both passes; 0 = no cap
}

// shareSource is the workspace or one repo, as the fill sees it.
type shareSource struct {
	label  string
	ranked []memory.RecallResult // recall order: pinned do-not-repeats first
	steps  [4]shareStep          // repos, full blocks: map modules, map dependencies, recent bugs, recent changes
	heads  [3][2]string          // map (steps 0–1), bugs, changes: text before and after the group's items, charged with its first item taken
}

// shareLine is one memory the block shows.
type shareLine struct {
	src  int // -1 the workspace, else the repo's index
	r    memory.RecallResult
	text string
}

// shareResult is what the division gives each part of the block.
type shareResult struct {
	framing  []string    // as given, "" where left out
	pins     []shareLine // the lines that always stay: the workspace's top pin, then each repo's
	lines    []shareLine // workspace's further pins and memories, then each repo's, in rank order
	sections []string    // per repo: its shown map, bugs and changes, rendered
	bugIDs   []string    // the recent bugs the sections show
}

// Where a placed memory shows, besides a source's index.
const (
	placedAlways = -2 // among the lines that always stay
	srcWorkspace = -1
)

// Forms a memory takes in a source's fill.
const (
	formNone = iota
	formTitle
	formFull
)

// cost is a text's share of the budget, rounded up so the costs of the parts
// bound the whole block.
func cost(s string) int { return ledger.EstimateTokensCeil(s) }

// stepGroup is the head group of fill step i.
func stepGroup(i int) int { return max(i-1, 0) }

// isPin reports whether r is a pinned do-not-repeat.
func isPin(r memory.RecallResult) bool {
	return r.Record.Kind == memory.KindDoNotRepeat && r.Record.Pinned
}

// memoryTexts returns r's line as a pin, in full, as the full gate measures it,
// and as a title, labelled with label.
func memoryTexts(r memory.RecallResult, label string) (pin, full, gate, title string) {
	prefix := fmt.Sprintf("- [%s, %s] %s\n", r.Record.Kind, label, headingTitle(r.Record))
	caveat := ""
	if c := r.Caveat(); c != "" {
		caveat = "  " + c + "\n"
	}
	return prefix + "  " + r.Record.Body + "\n" + caveat,
		prefix + "  " + trimOneLine(r.Record.Body) + "\n" + caveat,
		prefix + "  " + r.Record.Body + "\n" + caveat,
		prefix + caveat
}

// fillState is one source's progress through the fill.
type fillState struct {
	src      *shareSource
	idx      int
	taken    [4][]bool
	counted  [4]int
	shown    []bool // modules shown, taken or brought along
	paid     [3]bool
	topPin   string
	pinsUsed int
	pinCap   int
	form     map[string]int
}

func newFillState(src *shareSource, idx, pinCap int) *fillState {
	st := &fillState{src: src, idx: idx, pinCap: pinCap, form: map[string]int{}}
	for i := range src.steps {
		st.taken[i] = make([]bool, len(src.steps[i].items))
	}
	st.shown = make([]bool, len(src.steps[0].items))
	for _, r := range src.ranked {
		if isPin(r) {
			st.topPin = r.Record.ID
			break
		}
	}
	return st
}

// fill takes what fits room, step by step, skipping what does not fit and
// trying the next item. It only adds: nothing taken earlier is removed or shortened.
func (st *fillState) fill(room *int, steps bool, placed map[string]int) {
	src := st.src
	if steps {
		for si := range src.steps {
			st.fillStep(si, room, placed)
		}
	}
	for _, r := range src.ranked {
		id := r.Record.ID
		if !isPin(r) || id == st.topPin || st.form[id] != formNone {
			continue
		}
		if _, ok := placed[id]; ok {
			continue
		}
		pin, _, _, _ := memoryTexts(r, src.label)
		if c := cost(pin); c <= *room && st.pinsUsed+c <= st.pinCap {
			*room -= c
			st.pinsUsed += c
			st.form[id] = formFull
			placed[id] = st.idx
		}
	}
	for _, r := range src.ranked {
		id := r.Record.ID
		if isPin(r) {
			continue
		}
		_, full, gate, title := memoryTexts(r, src.label)
		g := max(cost(gate), cost(full))
		switch st.form[id] {
		case formFull:
		case formTitle:
			if g-cost(title) <= *room {
				*room -= cost(full) - cost(title)
				st.form[id] = formFull
			}
		default:
			if _, ok := placed[id]; ok {
				continue
			}
			switch {
			case g <= *room:
				*room -= cost(full)
				st.form[id] = formFull
			case cost(title) <= *room:
				*room -= cost(title)
				st.form[id] = formTitle
			default:
				continue
			}
			placed[id] = st.idx
		}
	}
}

// fillStep takes step si's items that fit room.
func (st *fillState) fillStep(si int, room *int, placed map[string]int) {
	src := st.src
	step := src.steps[si]
	g := stepGroup(si)
	for ii, it := range step.items {
		if st.taken[si][ii] || (si == 0 && st.shown[ii]) {
			continue
		}
		if it.id != "" {
			if _, ok := placed[it.id]; ok {
				continue
			}
		}
		if step.cap > 0 && st.counted[si] >= step.cap {
			continue
		}
		if it.last && slices.Contains(st.taken[si][:ii], false) {
			continue
		}
		c := cost(it.text)
		var ends []int
		for _, e := range it.ends {
			if !st.shown[e] {
				c += cost(src.steps[0].items[e].text)
				ends = append(ends, e)
			}
		}
		if !st.paid[g] {
			c += cost(src.heads[g][0]) + cost(src.heads[g][1])
		}
		if c > *room {
			continue
		}
		*room -= c
		st.taken[si][ii] = true
		st.counted[si]++
		st.paid[g] = true
		if si == 0 {
			st.shown[ii] = true
		}
		for _, e := range ends {
			st.shown[e] = true
		}
		if it.id != "" {
			placed[it.id] = st.idx
		}
	}
}

// section renders what the fill took of the source's map, bugs and changes.
func (st *fillState) section() (string, []string) {
	src := st.src
	var b strings.Builder
	var bugIDs []string
	for g := range 3 {
		var items []shareItem
		if g == 0 {
			for ii, it := range src.steps[0].items {
				if st.shown[ii] {
					items = append(items, it)
				}
			}
			mods := len(items)
			for ii, it := range src.steps[1].items {
				if st.taken[1][ii] {
					items = append(items, it)
				}
			}
			sort.SliceStable(items[:mods], func(i, j int) bool { return items[i].key < items[j].key })
			deps := items[mods:]
			sort.SliceStable(deps, func(i, j int) bool { return deps[i].key < deps[j].key })
		} else {
			for ii, it := range src.steps[g+1].items {
				if st.taken[g+1][ii] {
					items = append(items, it)
				}
			}
		}
		if len(items) == 0 {
			continue
		}
		b.WriteString(src.heads[g][0])
		for _, it := range items {
			b.WriteString(it.text)
			if it.id != "" {
				bugIDs = append(bugIDs, it.id)
			}
		}
		b.WriteString(src.heads[g][1])
	}
	return b.String(), bugIDs
}

// lines returns the further pins and memories the fill took, in rank order.
func (st *fillState) lines() []shareLine {
	var out []shareLine
	for _, r := range st.src.ranked {
		pin, full, _, title := memoryTexts(r, st.src.label)
		switch {
		case st.form[r.Record.ID] == formNone:
		case isPin(r):
			out = append(out, shareLine{st.idx, r, pin})
		case st.form[r.Record.ID] == formFull:
			out = append(out, shareLine{st.idx, r, full})
		default:
			out = append(out, shareLine{st.idx, r, title})
		}
	}
	return out
}

// share divides budget between the workspace and the repos: the top pins of
// each always stay, free; the workspace takes at most a third; the rest is cut
// into equal slices, one per repo, each filled in order; what the repos leave
// is offered once to each repo in turn; framing takes what is left after that.
// A resume charges a fixed reserve in place of each repo's map, bugs and
// changes, and keeps its framing as given.
func share(budget int, ws *shareSource, repos []*shareSource, framing []string, resume bool) shareResult {
	var res shareResult
	placed := map[string]int{}
	sources := slices.Clone(repos)
	idx := make([]int, len(repos))
	for i := range repos {
		idx[i] = i
	}
	if ws != nil {
		sources = append([]*shareSource{ws}, sources...)
		idx = append([]int{srcWorkspace}, idx...)
	}
	for i, src := range sources {
		for _, r := range src.ranked {
			if !isPin(r) {
				continue
			}
			// A source whose top pin another shows adds no second copy and no other pin.
			if _, ok := placed[r.Record.ID]; !ok {
				pin, _, _, _ := memoryTexts(r, src.label)
				res.pins = append(res.pins, shareLine{idx[i], r, pin})
				placed[r.Record.ID] = placedAlways
			}
			break
		}
	}

	wsUsed := 0
	if ws != nil {
		third := budget / 3
		room := third
		st := newFillState(ws, srcWorkspace, third/2)
		st.fill(&room, false, placed)
		wsUsed = third - room
		res.lines = append(res.lines, st.lines()...)
	}

	n := len(repos)
	states := make([]*fillState, n)
	pool := 0
	if n > 0 {
		part := budget - wsUsed
		for i, src := range repos {
			slice := part / n
			if i < part%n {
				slice++
			}
			room := slice
			if resume {
				room -= min(cerebrumReserve, slice)
			}
			states[i] = newFillState(src, i, slice/2)
			states[i].fill(&room, !resume, placed)
			pool += room
		}
		for _, st := range states {
			st.fill(&pool, !resume, placed)
		}
	}
	for _, st := range states {
		res.lines = append(res.lines, st.lines()...)
		sec, ids := st.section()
		res.sections = append(res.sections, sec)
		res.bugIDs = append(res.bugIDs, ids...)
	}

	res.framing = slices.Clone(framing)
	if !resume {
		left := pool
		for i, f := range res.framing {
			if c := cost(f); c <= left {
				left -= c
			} else {
				res.framing[i] = ""
			}
		}
	}
	return res
}

// Indexes of sharedBlock's framing texts.
const (
	fHeader = iota
	fEmpty
	fServed
	fGraph
	fNudge
)

// sharedBlock builds the block of a conversation that covers several repos:
// each repo's map, bugs, changes and memories, labelled with it, within shares
// of the budget (share).
func sharedBlock(ctx, skip context.Context, a *app.App, now int64, scope string, resumed bool,
	wait *startWait, session string, since int64, covered []coveredRepo) (Result, error) {
	codeMap := !resumed && a.Features.On(features.CodeMap)
	var tiers []*mapTier
	if codeMap {
		tiers = mapTiers(ctx, skip, wait, covered, a.StartIndex)
	} else {
		tiers = make([]*mapTier, len(covered))
	}
	// The wait used up the first bound; the writes after it get their own.
	skip = store.WithWriteDeadline(ctx, time.Now().Add(writeGrace))
	rcs := make([]*app.RepoContext, len(covered))
	for i, c := range covered {
		rcs[i] = c.RepoContext
	}
	sessionStartUpkeep(skip, a, rcs)

	f := toolFeatures(a)
	ws, srcs, reads, empty, err := shareSources(ctx, skip, a, now, resumed, session, since, covered, tiers, f)
	if err != nil {
		return Result{}, err
	}
	framing := sharedFraming(a, f, covered, empty, codeMap)
	res := share(sessionBudget(), ws, srcs, framing, resumed)
	chosen := append(slices.Clone(res.pins), res.lines...)
	if resumed {
		return sharedResume(ctx, skip, a, scope, framing[fHeader], chosen, covered)
	}

	render := func() string {
		var b strings.Builder
		b.WriteString(res.framing[fHeader])
		b.WriteString(res.framing[fEmpty])
		for _, l := range chosen {
			b.WriteString(l.text)
		}
		for i, c := range covered {
			b.WriteString(oneLine(c, tiers[i], codeMap))
			b.WriteString(res.sections[i])
		}
		b.WriteString(res.framing[fServed])
		b.WriteString(res.framing[fGraph])
		b.WriteString(res.framing[fNudge])
		return strings.TrimSpace(b.String())
	}
	out := render()
	ids := make([]string, 0, len(chosen)+len(res.bugIDs))
	for _, l := range chosen {
		ids = append(ids, l.r.Record.ID)
	}
	markSurfaced(ctx, a, scope, append(ids, res.bugIDs...))
	// A lost write only costs the next resume a full block, and the next start
	// may list a change of this block again; it never hides one.
	_ = recordBlock(ctx, a, scope, blockMarks{session: session, roster: true}, reads)
	noteRecalled(skip, a, covered, chosen)
	// An index can finish while the marks wait for its writer; only the one
	// lines change, so the marks just written still match.
	changed := false
	for _, t := range tiers {
		changed = t.recheck(ctx, skip) || changed
	}
	if changed {
		out = render()
	}
	return Result{Context: out}, nil
}

// shareSources gathers what share divides: the workspace's ranked memories,
// and each covered repo's ranked memories and, for a full block, its map,
// bugs and changes. reads are the journal marks the full block read per repo.
// empty reports that no source holds a memory.
func shareSources(ctx, skip context.Context, a *app.App, now int64, resumed bool, session string, since int64,
	covered []coveredRepo, tiers []*mapTier, f features.Set) (ws *shareSource, srcs []*shareSource, reads []repoRead, empty bool, err error) {
	memories := a.Features.On(features.SessionMemories)
	keep := func(rs []memory.RecallResult) []memory.RecallResult {
		if a.Features.On(features.Todos) {
			return rs
		}
		return slices.DeleteFunc(rs, func(r memory.RecallResult) bool { return r.Record.Kind == memory.KindTodo })
	}
	empty = true
	if memories && a.Workspace != nil {
		// As in a one-repo block, a workspace that cannot be read leaves its tier out.
		if ranked, err := a.Workspace.Engine.Ranked(skip, memory.RecallQuery{Baseline: true}); err == nil {
			ws = &shareSource{label: "workspace " + a.Workspace.Name, ranked: keep(ranked)}
			empty = len(ws.ranked) == 0
		}
	}
	srcs = make([]*shareSource, len(covered))
	for i, c := range covered {
		src := &shareSource{label: c.label()}
		if memories {
			ranked, err := c.Engine.Ranked(skip, memory.RecallQuery{Branch: c.Git.Branch, Worktree: c.Git.Worktree, Baseline: true})
			if err != nil {
				return nil, nil, nil, false, err
			}
			src.ranked = keep(ranked)
			empty = empty && len(src.ranked) == 0
		}
		fr := f
		if c.sel == "" {
			fr = withoutTools(f)
		}
		r, read := recentChanges(ctx, skip, a, c.RepoContext, graph.Session{ID: session, StartedAt: since, Mark: c.blockMark}, now)
		if read && r.Base != session {
			reads = append(reads, repoRead{wt: c.Git.Worktree, mark: r.Read})
		}
		if !resumed {
			arch, line := tiers[i].result()
			if line == "" && len(arch.Modules) > 0 {
				mapSteps(src, arch)
			}
			if a.Features.On(features.RecentBugs) {
				bugs, _ := c.Engine.RecentBugs(ctx, c.Git.Branch, -1)
				bugSteps(src, bugs, fr.On(features.MemoryTools))
			}
			changeSteps(src, buildDeltaTier(r.Deltas, r.Total, r.Since, fr.On(features.GraphTools)))
		}
		srcs[i] = src
	}
	return ws, srcs, reads, empty, nil
}

// sharedFraming returns the several-repo block's header and closing texts,
// indexed by fHeader … fNudge, "" where the features leave one out.
func sharedFraming(a *app.App, f features.Set, covered []coveredRepo, empty, codeMap bool) []string {
	memories := a.Features.On(features.SessionMemories)
	framing := make([]string, fNudge+1)
	if memories {
		framing[fHeader] = "Recarto memory for the served repos:\n"
		if empty {
			framing[fEmpty] = "(no prior memories yet)\n"
		}
	}
	var sels []string
	for _, c := range covered {
		if c.sel != "" {
			sels = append(sels, c.sel)
		}
	}
	if len(sels) > 0 && (f.On(features.GraphTools) || f.On(features.MemoryTools)) {
		framing[fServed] = "\nGraph and memory tools take repo:\"<name>\" with one of: " + strings.Join(sels, ", ") +
			"; recall and file-anchored surfacing cover the served repos.\n"
	}
	if codeMap && f.On(features.GraphTools) {
		framing[fGraph] = "Query the code graph with the recarto MCP tools (search, snippet, callers) instead of reading whole files; grep/glob auto-injects matching symbols.\n"
	}
	if memories && f.On(features.MemoryTools) {
		framing[fNudge] = "\nRecord new learnings with the recarto MCP tool `record_memory` and bug fixes with `record_bug`."
	}
	return framing
}

// sharedResume shows the chosen memories scope's conversation has not been
// shown yet, under header, and marks them shown.
func sharedResume(ctx, skip context.Context, a *app.App, scope, header string, chosen []shareLine, covered []coveredRepo) (Result, error) {
	var shown []shareLine
	for _, l := range chosen {
		n, err := a.Ledger.Get(ctx, scope, shownMetric+l.r.Record.ID)
		if err != nil {
			return Result{}, err
		}
		if n == 0 {
			shown = append(shown, l)
		}
	}
	if len(shown) == 0 {
		return Result{}, nil
	}
	var b strings.Builder
	b.WriteString(header)
	ids := make([]string, 0, len(shown))
	for _, l := range shown {
		b.WriteString(l.text)
		ids = append(ids, l.r.Record.ID)
	}
	markSurfaced(ctx, a, scope, ids)
	noteRecalled(skip, a, covered, shown)
	return Result{Context: strings.TrimSpace(b.String())}, nil
}

// noteRecalled counts each shown memory as recalled by the engine of the
// source that shows it.
func noteRecalled(ctx context.Context, a *app.App, covered []coveredRepo, shown []shareLine) {
	bySrc := map[int][]memory.RecallResult{}
	for _, l := range shown {
		bySrc[l.src] = append(bySrc[l.src], l.r)
	}
	for src, rs := range bySrc {
		if src == srcWorkspace {
			a.Workspace.Engine.NoteRecalled(ctx, rs)
		} else {
			covered[src].Engine.NoteRecalled(ctx, rs)
		}
	}
}

// oneLine is the line naming covered repo c in a several-repo block: its label
// with its file and symbol counts, or the line standing in for its map.
func oneLine(c coveredRepo, t *mapTier, codeMap bool) string {
	if !codeMap || t == nil {
		return "\n" + c.label() + "\n"
	}
	arch, line := t.result()
	if line != "" {
		return "\n" + c.label() + ": " + line + "\n"
	}
	if !t.mapped() {
		// An index cut short with none running: no counts are known.
		return "\n" + c.label() + "\n"
	}
	files, syms := 0, 0
	for _, m := range arch.Modules {
		files += m.Files
		syms += m.Symbols
	}
	return fmt.Sprintf("\n%s: %d files, %d symbols\n", c.label(), files, syms)
}

// mapSteps fills src's map steps from arch: the code map's modules and
// dependencies first, in the order it lists them, then the rest in the map's
// ranking. A module the map shows only as a dependency's endpoint is not among
// its modules: past the cap it enters only beside a dependency that is shown.
func mapSteps(src *shareSource, arch graph.Architecture) {
	mods, deps := graph.MapArchitecture(arch, archModuleLimit)
	rmods, rdeps := graph.RankArchitecture(arch)
	ranked := map[string]bool{}
	for _, m := range rmods[:min(archModuleLimit, len(rmods))] {
		ranked[m.Name] = true
	}
	index := map[string]int{}
	modItem := func(m graph.Module) {
		index[m.Name] = len(src.steps[0].items)
		src.steps[0].items = append(src.steps[0].items, shareItem{
			text: fmt.Sprintf("- %s: %d files, %d symbols\n", m.Name, m.Files, m.Symbols),
			key:  m.Name,
		})
	}
	for _, m := range mods {
		if ranked[m.Name] {
			modItem(m)
		}
	}
	for _, m := range rmods {
		if _, ok := index[m.Name]; !ok {
			modItem(m)
		}
	}
	type edge struct{ from, to string }
	inMap := map[edge]bool{}
	depItem := func(d graph.ModuleEdge) {
		var ends []int
		for _, end := range [2]string{d.From, d.To} {
			if i, ok := index[end]; ok {
				ends = append(ends, i)
			}
		}
		src.steps[1].items = append(src.steps[1].items, shareItem{
			text: fmt.Sprintf("  %s → %s (%d)\n", d.From, d.To, d.Count),
			key:  d.From + "\x00" + d.To,
			ends: ends,
		})
	}
	for _, d := range deps {
		inMap[edge{d.From, d.To}] = true
		depItem(d)
	}
	for _, d := range rdeps {
		if !inMap[edge{d.From, d.To}] {
			depItem(d)
		}
	}
	src.steps[0].cap, src.steps[1].cap = archModuleLimit, archModuleLimit
	src.heads[0] = [2]string{"\nCode map (modules):\n", ""}
}

// bugSteps fills src's recent-bugs step: every bug, newest first.
func bugSteps(src *shareSource, bugs []*memory.Record, pointer bool) {
	for _, b := range bugs {
		src.steps[2].items = append(src.steps[2].items, shareItem{text: fmt.Sprintf("- %s\n", noteTitle(b)), id: b.ID})
	}
	src.steps[2].cap = recentBugLimit
	head := "\nRecent bugs:\n"
	if pointer {
		head = "\nRecent bugs (recarto MCP tool `search_bugs` for detail):\n"
	}
	src.heads[1] = [2]string{head, ""}
}

// changeSteps fills src's recent-changes step from the changes tier: its module
// lines, then its "more" line once every module line is taken.
func changeSteps(src *shareSource, t deltaTier) {
	if t.head == "" {
		return
	}
	if len(t.lines) == 0 {
		// The one-line summary is one item.
		src.steps[3].items = []shareItem{{text: t.head + t.more + t.tail}}
		return
	}
	for _, l := range t.lines {
		src.steps[3].items = append(src.steps[3].items, shareItem{text: l})
	}
	if t.more != "" {
		src.steps[3].items = append(src.steps[3].items, shareItem{text: t.more, last: true})
	}
	src.heads[2] = [2]string{t.head, t.tail}
}
