package agents

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gerwim/recarto/internal/graph"
	"github.com/gerwim/recarto/internal/ledger"
	"github.com/gerwim/recarto/internal/memory"
)

// sized is a text that costs exactly n tokens, tagged so a test can find it.
func sized(tag string, n int) string {
	l := int(float64(n) * 3.8)
	if l < len(tag) {
		panic("tag longer than its text")
	}
	s := tag + strings.Repeat(".", l-len(tag))
	if cost(s) != n {
		panic(fmt.Sprintf("sized(%q, %d) costs %d", tag, n, cost(s)))
	}
	return s
}

// items returns one item per text.
func items(texts ...string) []shareItem {
	out := make([]shareItem, len(texts))
	for i, s := range texts {
		out[i] = shareItem{text: s}
	}
	return out
}

// mem is a ranked memory of kind with a body of about bodyLen characters.
func mem(id string, kind memory.Kind, pinned bool, title string, bodyLen int) memory.RecallResult {
	return memory.RecallResult{Record: &memory.Record{ID: id, Kind: kind, Pinned: pinned, Title: title, Body: strings.Repeat("b", bodyLen)}}
}

// rendered is the result as one text.
func rendered(res shareResult) string {
	var b strings.Builder
	for _, f := range res.framing {
		b.WriteString(f)
	}
	for _, l := range res.pins {
		b.WriteString(l.text)
	}
	for _, l := range res.lines {
		b.WriteString(l.text)
	}
	for _, s := range res.sections {
		b.WriteString(s)
	}
	return b.String()
}

// costs are a memory's costs with label.
func costs(r memory.RecallResult, label string) (pin, full, gate, title int) {
	p, f, g, t := memoryTexts(r, label)
	return cost(p), cost(f), max(cost(g), cost(f)), cost(t)
}

func TestShareSkipsAnItemThatDoesNotFit(t *testing.T) {
	a := &shareSource{label: "a"}
	a.steps[0].items = items(sized("MAP", 100))
	a.steps[2] = shareStep{items: items(sized("NEW", 70), sized("OLD", 30)), cap: 5}
	b := &shareSource{label: "b"}
	b.steps[0].items = items(sized("B", 110))
	res := share(300, nil, []*shareSource{a, b}, nil, false)
	if !strings.Contains(res.sections[0], "OLD") || strings.Contains(res.sections[0], "NEW") {
		t.Fatalf("want the older bug and not the newest:\n%s", res.sections[0])
	}
}

func TestShareMemoryFillRule(t *testing.T) {
	f1 := mem("F-1", memory.KindLearning, false, "F-1", 600)
	f2 := mem("F-2", memory.KindLearning, false, "F-2", 180)
	f3 := mem("F-3", memory.KindLearning, false, "F-3 "+strings.Repeat("t", 90), 10)
	_, _, g1, t1 := costs(f1, "a")
	_, full2, g2, _ := costs(f2, "a")
	_, _, g3, t3 := costs(f3, "a")
	room := t1 + full2 + t3 - 5
	if g1 <= room || g2 > room-t1 || g3 <= t3-5 {
		t.Fatalf("fixture sizes: g1 %d g2 %d g3 %d t3 %d room %d", g1, g2, g3, t3, room)
	}
	a := &shareSource{label: "a", ranked: []memory.RecallResult{f1, f2, f3}}
	b := &shareSource{label: "b"}
	b.steps[0].items = items(sized("B", room-4))
	res := share(2*room, nil, []*shareSource{a, b}, nil, false)
	var got []string
	for _, l := range res.lines {
		_, full, _, _ := memoryTexts(l.r, "a")
		what := "title"
		if l.text == full {
			what = "full"
		}
		got = append(got, l.r.Record.ID+":"+what)
	}
	if want := []string{"F-1:title", "F-2:full"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lines %v, want %v", got, want)
	}
}

func TestSharePoolGoesWholeInOrder(t *testing.T) {
	a := &shareSource{label: "a"}
	a.steps[0].items = items(sized("A", 100))
	b := &shareSource{label: "b"}
	b.steps[0].items = items(sized("B", 190))
	b.steps[2].items = items(sized("BBUG", 60))
	c := &shareSource{label: "c"}
	c.steps[0].items = items(sized("C", 195))
	c.steps[2].items = items(sized("CBUG", 60))
	res := share(600, nil, []*shareSource{a, b, c}, []string{sized("LEFT", 55)}, false)
	if !strings.Contains(res.sections[1], "BBUG") || strings.Contains(res.sections[2], "CBUG") {
		t.Fatalf("want b's bug back and not c's:\n%v", res.sections)
	}
	if res.framing[0] == "" {
		t.Errorf("the 55 tokens left should stay unused, for the framing")
	}
}

func TestShareRepoThatFedThePool(t *testing.T) {
	for name, tc := range map[string]struct {
		second int
		bug    bool
	}{"pool 90": {60, true}, "pool 50": {100, false}} {
		a := &shareSource{label: "a"}
		a.steps[0].items = items(sized("A", 50))
		a.steps[2].items = items(sized("ABUG", 80))
		b := &shareSource{label: "b"}
		b.steps[0].items = items(sized("B", tc.second))
		c := &shareSource{label: "c"}
		c.steps[0].items = items(sized("C", 100))
		res := share(300, nil, []*shareSource{a, b, c}, nil, false)
		if got := strings.Contains(res.sections[0], "ABUG"); got != tc.bug {
			t.Errorf("%s: bug shown %v, want %v", name, got, tc.bug)
		}
		if n := ledger.EstimateTokens(rendered(res)); n > 300 {
			t.Errorf("%s: repos use %d tokens, over 300", name, n)
		}
	}
}

func TestShareTitleBecomesFull(t *testing.T) {
	t1 := mem("T-1", memory.KindLearning, false, "T-1", 420)
	_, full, gate, title := costs(t1, "b")
	a := &shareSource{label: "a"}
	a.steps[0].items = items(sized("A", 50))
	b := &shareSource{label: "b", ranked: []memory.RecallResult{t1}}
	b.steps[0].items = items(sized("B", 200-title-2))
	if gate <= title+2 || gate-title > 150 {
		t.Fatalf("fixture: gate %d title %d", gate, title)
	}
	res := share(400, nil, []*shareSource{a, b}, nil, false)
	if len(res.lines) != 1 || cost(res.lines[0].text) != full {
		t.Fatalf("want T-1 in full: %+v", res.lines)
	}
}

// furtherPin returns a repo ranked with a small top pin and a second pin of
// about n tokens.
func furtherPin(label string, n int) (*shareSource, memory.RecallResult) {
	top := mem("TOP-"+label, memory.KindDoNotRepeat, true, "top", 10)
	second := mem("PIN2-"+label, memory.KindDoNotRepeat, true, "pin2", int(float64(n)*3.8)-20)
	return &shareSource{label: label, ranked: []memory.RecallResult{top, second}}, second
}

func TestShareFurtherPinAgainstTheMap(t *testing.T) {
	for _, withMap := range []bool{true, false} {
		a, pin2 := furtherPin("a", 140)
		p, _, _, _ := costs(pin2, "a")
		if p <= 110 || p > 150 {
			t.Fatalf("fixture: pin costs %d", p)
		}
		if withMap {
			a.steps[0].items = items(sized("MAP", 165))
		}
		a.steps[2].items = items(sized("BUG", 25))
		b := &shareSource{label: "b"}
		b.steps[0].items = items(sized("B", 280))
		res := share(600, nil, []*shareSource{a, b}, nil, false)
		shown := false
		for _, l := range res.lines {
			if l.r.Record.ID == pin2.Record.ID {
				shown = true
				if !strings.Contains(l.text, pin2.Record.Body) {
					t.Errorf("pin shortened")
				}
			}
		}
		if shown == withMap {
			t.Errorf("map %v: second pin shown %v", withMap, shown)
		}
		if withMap && !strings.Contains(res.sections[0], "MAP") {
			t.Errorf("map left out")
		}
	}
}

func TestShareHalfSliceCapOnFurtherPins(t *testing.T) {
	a, pin2 := furtherPin("a", 160)
	a.steps[0].items = items(sized("MAP", 100))
	b := &shareSource{label: "b"}
	res := share(600, nil, []*shareSource{a, b}, nil, false)
	for _, l := range res.lines {
		if l.r.Record.ID == pin2.Record.ID {
			t.Fatalf("a pin over half the slice was shown, with a pool of 300")
		}
	}
}

func TestShareBudgetSmallerThanTheLinesThatAlwaysStay(t *testing.T) {
	ws := &shareSource{label: "workspace w", ranked: []memory.RecallResult{mem("WS", memory.KindDecision, false, "WS-DECISION", 40)}}
	var repos []*shareSource
	for _, l := range []string{"a", "b"} {
		src, _ := furtherPin(l, 20)
		src.ranked = append(src.ranked, mem("L-"+l, memory.KindLearning, false, "a learning whose title alone is longer than ten tokens", 20))
		src.steps[0].items = items(sized("MAP"+l, 20))
		repos = append(repos, src)
	}
	res := share(10, ws, repos, nil, false)
	if len(res.pins) != 2 || res.pins[0].r.Record.ID != "TOP-a" || res.pins[1].r.Record.ID != "TOP-b" {
		t.Fatalf("pins %+v, want both repos' top pins", res.pins)
	}
	if len(res.lines) != 0 || res.sections[0] != "" || res.sections[1] != "" {
		t.Fatalf("lines %+v sections %q, want nothing else", res.lines, res.sections)
	}
}

func TestShareOversizedWorkspacePinTakesNothing(t *testing.T) {
	repos := func() []*shareSource {
		var out []*shareSource
		for _, l := range []string{"a", "b"} {
			src := &shareSource{label: l}
			for i := range 10 {
				src.ranked = append(src.ranked, mem(fmt.Sprintf("%s-%d", l, i), memory.KindLearning, false, "learning", 560))
			}
			src.steps[0].items = items(sized("MAP"+l, 80))
			out = append(out, src)
		}
		return out
	}
	decision := mem("WS", memory.KindDecision, false, "WS-DECISION", 40)
	big := mem("BIG", memory.KindDoNotRepeat, true, "big", 3040)
	plain := share(900, &shareSource{label: "w", ranked: []memory.RecallResult{decision}}, repos(), nil, false)
	pinned := share(900, &shareSource{label: "w", ranked: []memory.RecallResult{big, decision}}, repos(), nil, false)
	if len(pinned.pins) != 1 || pinned.pins[0].r.Record.ID != "BIG" {
		t.Fatalf("pins %+v, want the workspace pin", pinned.pins)
	}
	if !reflect.DeepEqual(plain.lines, pinned.lines) || !reflect.DeepEqual(plain.sections, pinned.sections) {
		t.Fatalf("the workspace pin changed the rest:\n%+v\n%+v", plain.lines, pinned.lines)
	}
	if pinned.lines[0].r.Record.ID != "WS" {
		t.Errorf("WS-DECISION left out")
	}
}

func TestShareOneMemoryInTwoStores(t *testing.T) {
	shared := mem("SHARED", memory.KindLearning, false, "shared", 150)
	_, sharedFull, _, _ := costs(shared, "app-2")
	own := mem("OWN-2", memory.KindLearning, false, "own", 150)
	_, ownFull, _, _ := costs(own, "app-2")
	a := &shareSource{label: "app-1", ranked: []memory.RecallResult{shared}}
	b := &shareSource{label: "app-2", ranked: []memory.RecallResult{shared, own}}
	slice := ownFull + sharedFull/2
	a.steps[0].items = items(sized("A", slice-sharedFull))
	res := share(2*slice, nil, []*shareSource{a, b}, nil, false)
	var got []string
	for _, l := range res.lines {
		got = append(got, fmt.Sprintf("%s@%d", l.r.Record.ID, l.src))
	}
	if want := []string{"SHARED@0", "OWN-2@1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lines %v, want %v", got, want)
	}
	if !strings.Contains(res.lines[1].text, "own") || !strings.Contains(res.lines[1].text, "b\n") {
		t.Errorf("app-2's own learning not in full: %q", res.lines[1].text)
	}
}

func TestShareSharedPinShowsOnceAsTopPin(t *testing.T) {
	d := mem("D", memory.KindDoNotRepeat, true, "D", 200)
	p1 := mem("P1", memory.KindDoNotRepeat, true, "P1", 20)
	l1 := mem("L1", memory.KindLearning, false, "L1", 200)
	_, l1Full, _, _ := costs(l1, "app-1")
	a := &shareSource{label: "app-1", ranked: []memory.RecallResult{p1, d, l1}}
	a.steps[0].items = items(sized("MAPA", 50))
	b := &shareSource{label: "app-2", ranked: []memory.RecallResult{d}}
	b.steps[0].items = items(sized("MAPB", 100))
	res := share(2*(50+l1Full+5), nil, []*shareSource{a, b}, nil, false)
	var pins []string
	for _, l := range res.pins {
		pins = append(pins, fmt.Sprintf("%s@%d", l.r.Record.ID, l.src))
	}
	if want := []string{"P1@0", "D@1"}; !reflect.DeepEqual(pins, want) {
		t.Fatalf("pins %v, want %v", pins, want)
	}
	if len(res.lines) != 1 || res.lines[0].r.Record.ID != "L1" {
		t.Fatalf("lines %+v, want L1 alone", res.lines)
	}
}

func TestShareResumeReserveAndPoolOrder(t *testing.T) {
	learn := func(id string) memory.RecallResult { return mem(id, memory.KindLearning, false, id, 100) }
	ra := &shareSource{label: "ra", ranked: []memory.RecallResult{learn("RA-1"), learn("RA-2")}}
	rb := &shareSource{label: "rb"}
	rc := &shareSource{label: "rc"}
	for i := 4; i >= 1; i-- {
		rb.ranked = append(rb.ranked, learn(fmt.Sprintf("RB-%d", i)))
		rc.ranked = append(rc.ranked, learn(fmt.Sprintf("RC-%d", i)))
	}
	_, full, gate, title := costs(learn("RB-1"), "rb")
	if gate != full || title < 4 {
		t.Fatalf("fixture: full %d gate %d title %d", full, gate, title)
	}
	slice := cerebrumReserve + 3*full + 1
	res := share(3*slice, nil, []*shareSource{ra, rb, rc}, nil, true)
	var got []string
	for _, l := range res.lines {
		got = append(got, l.r.Record.ID)
	}
	want := []string{"RA-1", "RA-2", "RB-4", "RB-3", "RB-2", "RB-1", "RC-4", "RC-3", "RC-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resume selection %v, want %v", got, want)
	}
}

func TestShareStaysWithinTheBudget(t *testing.T) {
	arch := graph.Architecture{}
	for i := range 12 {
		arch.Modules = append(arch.Modules, graph.Module{Name: fmt.Sprintf("mod%02d", i), Files: 12 - i, Symbols: 40})
		arch.Deps = append(arch.Deps, graph.ModuleEdge{From: fmt.Sprintf("mod%02d", i), To: fmt.Sprintf("mod%02d", (i+1)%12), Count: 3 + i})
	}
	dense := func(l string) *shareSource {
		src := &shareSource{label: l}
		src.ranked = append(src.ranked, mem("TOP-"+l, memory.KindDoNotRepeat, true, "top", 80))
		src.ranked = append(src.ranked, mem("PIN-"+l, memory.KindDoNotRepeat, true, "pin", 120))
		for i := range 12 {
			src.ranked = append(src.ranked, mem(fmt.Sprintf("%s-%d", l, i), memory.KindLearning, false, "a learning title", 50+i*40))
		}
		mapSteps(src, arch)
		var bugs []*memory.Record
		for i := range 7 {
			bugs = append(bugs, &memory.Record{ID: fmt.Sprintf("bug-%s-%d", l, i), Title: strings.Repeat("bug ", 5+i*6)})
		}
		bugSteps(src, bugs, true)
		changeSteps(src, deltaTier{head: "\nChanged since (Jan 2):\n", lines: []string{"- a: +1\n", "- b: +2\n", "- c: ~3\n"}, more: "- … 4 more modules\n", tail: "(pointer)\n"})
		return src
	}
	for budget := 10; budget <= 3000; budget += 37 {
		ws := &shareSource{label: "workspace w", ranked: []memory.RecallResult{
			mem("WSPIN", memory.KindDoNotRepeat, true, "ws pin", 900), mem("WS2", memory.KindDoNotRepeat, true, "ws pin 2", 100),
			mem("WS", memory.KindDecision, false, "WS-DECISION", 200)}}
		res := share(budget, ws, []*shareSource{dense("a"), dense("b"), dense("c")},
			[]string{"Recarto memory for the served repos:\n", "\nGraph and memory tools take repo …\n"}, false)
		res.pins = nil
		if n := ledger.EstimateTokens(rendered(res)); n > budget {
			t.Fatalf("budget %d: block without its always-stay lines is %d tokens", budget, n)
		}
	}
}

func TestShareSlicesAndFraming(t *testing.T) {
	ones := func(n int) []shareItem {
		var out []shareItem
		for i := range n {
			out = append(out, shareItem{text: fmt.Sprintf("%02d", i%100)})
		}
		return out
	}
	a, b := &shareSource{label: "a"}, &shareSource{label: "b"}
	a.steps[3].items, b.steps[3].items = ones(200), ones(200)
	share(101, nil, []*shareSource{a, b}, nil, false)
	res := share(101, nil, []*shareSource{a, b}, nil, false)
	if n, m := strings.Count(res.sections[0], "")-1, strings.Count(res.sections[1], "")-1; n != 2*51 || m != 2*50 {
		t.Fatalf("slices took %d and %d tokens of items, want 51 and 50", n/2, m/2)
	}
	a.steps[3].items, b.steps[3].items = ones(40), ones(40)
	res = share(101, nil, []*shareSource{a, b}, []string{sized("BIG", 30), sized("SMALL", 10)}, false)
	if res.framing[0] != "" || res.framing[1] == "" {
		t.Fatalf("with 21 tokens left: framing %q", res.framing)
	}
}

func TestShareRecentBugs(t *testing.T) {
	bugs := func(sizes ...int) []shareItem {
		var out []shareItem
		for i, n := range sizes {
			out = append(out, shareItem{text: sized(fmt.Sprintf("BUG%d", i), n), id: fmt.Sprintf("bug%d", i)})
		}
		return out
	}
	a := &shareSource{label: "a"}
	a.steps[2] = shareStep{items: bugs(100, 100, 100, 100, 100, 10), cap: 5}
	res := share(50, nil, []*shareSource{a}, nil, false)
	if !strings.Contains(res.sections[0], "BUG5") {
		t.Errorf("a sixth bug that fits was not shown: %q", res.sections[0])
	}
	a.steps[2] = shareStep{items: bugs(5, 5, 5, 5, 5, 5, 5), cap: 5}
	res = share(500, nil, []*shareSource{a}, nil, false)
	if n := len(res.bugIDs); n != 5 {
		t.Errorf("ample room: %d bugs, want 5", n)
	}

	shared := memory.RecallResult{Record: &memory.Record{ID: "bug0", Kind: memory.KindBug, Title: "shared bug"}}
	a = &shareSource{label: "a"}
	a.steps[2] = shareStep{items: bugs(20), cap: 5}
	b := &shareSource{label: "b", ranked: []memory.RecallResult{shared}}
	b.steps[2] = shareStep{items: append(bugs(20), shareItem{text: sized("OTHER", 25), id: "other"}), cap: 5}
	res = share(60, nil, []*shareSource{a, b}, nil, false)
	if !strings.Contains(res.sections[0], "BUG0") || strings.Contains(res.sections[1], "BUG0") || !strings.Contains(res.sections[1], "OTHER") {
		t.Errorf("a bug two repos hold: sections %q", res.sections)
	}
	if len(res.lines) != 0 {
		t.Errorf("a bug shown as a recent bug was shown again as a memory: %+v", res.lines)
	}
}

func TestShareMap(t *testing.T) {
	arch := graph.Architecture{Modules: []graph.Module{{Name: "a", Files: 1, Symbols: 1}, {Name: "z", Files: 5, Symbols: 1}}}
	src := &shareSource{label: "r"}
	mapSteps(src, arch)
	one := cost(src.heads[0][0]) + cost(src.steps[0].items[0].text)
	res := share(one, nil, []*shareSource{src}, nil, false)
	if !strings.Contains(res.sections[0], "- a:") || strings.Contains(res.sections[0], "- z:") {
		t.Fatalf("room for one module: %q", res.sections[0])
	}

	// Nine modules, the ninth only an endpoint of a heavy edge: today's map backfills it.
	arch = graph.Architecture{}
	for i := range 9 {
		arch.Modules = append(arch.Modules, graph.Module{Name: fmt.Sprintf("m%d", i), Files: 20 - i, Symbols: 3})
	}
	arch.Deps = []graph.ModuleEdge{{From: "m0", To: "m8", Count: 50}, {From: "m1", To: "m2", Count: 5}}
	src = &shareSource{label: "r"}
	mapSteps(src, arch)
	res = share(5000, nil, []*shareSource{src}, nil, false)
	mods, deps := graph.MapArchitecture(arch, archModuleLimit)
	var want strings.Builder
	want.WriteString("\nCode map (modules):\n")
	for _, m := range mods {
		fmt.Fprintf(&want, "- %s: %d files, %d symbols\n", m.Name, m.Files, m.Symbols)
	}
	for _, d := range deps {
		fmt.Fprintf(&want, "  %s → %s (%d)\n", d.From, d.To, d.Count)
	}
	if res.sections[0] != want.String() {
		t.Fatalf("ample room:\n%s\nwant today's map:\n%s", res.sections[0], want.String())
	}

	// With the module cap reached after m0, the edge m0 → m1 brings m1 along.
	src = &shareSource{label: "r"}
	mapSteps(src, graph.Architecture{Modules: arch.Modules[:2], Deps: []graph.ModuleEdge{{From: "m0", To: "m1", Count: 1}}})
	src.steps[0].cap = 1
	room := cost(src.heads[0][0]) + cost(src.steps[0].items[0].text) + cost(src.steps[0].items[1].text) + cost(src.steps[1].items[0].text)
	res = share(room, nil, []*shareSource{src}, nil, false)
	if !strings.Contains(res.sections[0], "- m1:") || !strings.Contains(res.sections[0], "m0 → m1") {
		t.Fatalf("the edge did not bring its endpoint: %q", res.sections[0])
	}
}

func TestShareChanges(t *testing.T) {
	src := &shareSource{label: "r"}
	changeSteps(src, deltaTier{head: "\nChanged:\n", lines: []string{sized("BIG", 50), sized("SMALL", 5)}, more: "- … 3 more modules\n", tail: "(pointer)\n"})
	room := cost("\nChanged:\n") + cost("(pointer)\n") + 10
	res := share(room, nil, []*shareSource{src}, nil, false)
	got := res.sections[0]
	if strings.Contains(got, "BIG") || !strings.Contains(got, "SMALL") || strings.Contains(got, "more modules") {
		t.Fatalf("changes: %q", got)
	}
	if strings.Count(got, "Changed:") != 1 || !strings.HasSuffix(got, "(pointer)\n") {
		t.Errorf("head and pointer once around the lines: %q", got)
	}
	res = share(1000, nil, []*shareSource{src}, nil, false)
	if !strings.Contains(res.sections[0], "more modules") {
		t.Errorf("every line taken: want the more line: %q", res.sections[0])
	}
}
