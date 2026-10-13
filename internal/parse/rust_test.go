package parse

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

const rustSrc = `use std::fmt::{self, Display};
use crate::geo::Point as P;
mod inner { pub fn helper() {} }
mod decl;
/// docs
#[derive(Debug, Clone)]
pub struct Circle<T> { r: T }
pub union Bits { a: u32 }
pub enum Shape { Round(f64), Empty }
type Alias = Circle<f64>;
pub trait Area: Display { fn area(&self) -> f64; fn twice(&self) -> f64 { self.area() * 2.0 } }
impl<T> Circle<T> { pub fn new(r: T) -> Self { Self::make(r) } fn make(r: T) -> Self { Circle { r } } }
impl fmt::Display for Circle<f64> { fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result { write!(f, "{}", self.area()) } }
macro_rules! sq { ($x:expr) => { $x * $x }; }
extern "C" { fn abs(x: i32) -> i32; }
#[test]
// a comment between attribute and item
fn run(s: &dyn Area, m: &mut Circle<f64>) {
    let x: Circle<f64> = Circle::new(1.0);
    let y = Circle { r: 2.0 };
    let z = Circle::<f64>::default();
    fn nested() {}
    assert_eq!(compute(foo.bar(1), inner::helper()), y, (1, 2), vec!(1));
    x.area();
    super::up();
}
`

// TestParseRust pins the Rust extraction end to end on one file: every item
// kind exactly once (no function_item double-captured as function and method),
// attribute-widened spans, every edge kind, and the receiver bindings.
func TestParseRust(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	res, err := r.ParseFile("rust", "src/lib.rs", []byte(rustSrc))
	if err != nil {
		t.Fatal(err)
	}

	var syms []string
	for _, s := range res.Symbols {
		syms = append(syms, fmt.Sprintf("%s %s %d-%d@%d", s.Kind, s.Name, s.StartLine, s.EndLine, s.NameLine))
	}
	wantSyms := []string{
		"module inner 3-3@3", "function helper 3-3@3",
		"struct Circle 6-7@7", // span widened over #[derive], not the doc comment
		"struct Bits 8-8@8",
		"enum Shape 9-9@9", "enum_member Round 9-9@9", "enum_member Empty 9-9@9",
		"type Alias 10-10@10",
		"trait Area 11-11@11", "method area 11-11@11", "method twice 11-11@11",
		"impl Circle 12-12@12", "method new 12-12@12", "method make 12-12@12",
		"impl Circle 13-13@13", "method fmt 13-13@13",
		"macro sq 14-14@14",
		"function abs 15-15@15",
		"function run 16-26@18", // #[test] widened across the comment
		"function nested 22-22@22",
	}
	if !slices.Equal(syms, wantSyms) {
		t.Errorf("symbols:\n got %s\nwant %s", strings.Join(syms, ", "), strings.Join(wantSyms, ", "))
	}

	var edges []string
	for _, e := range res.Edges {
		edges = append(edges, fmt.Sprintf("%s %s %s %d", e.Kind, e.DstName, e.Recv, e.Line))
	}
	wantEdges := []string{
		"imports Display  1", "imports Point  2",
		"annotates derive  6", "annotates Debug  6", "annotates Clone  6",
		"extends Display  11", "calls area self 11",
		"calls make Self 12",
		"extends Display  13", "references write  13", "calls area self 13",
		"annotates test  16",
		"calls new Circle 19",
		"calls default Circle 21",
		// inside assert_eq!: compute, foo.bar and inner::helper are calls; `y, (1, 2)`
		// and the nested vec!(1) are not (punctuation separates them)
		"references assert_eq  23", "calls compute  23", "calls bar foo 23", "calls helper inner 23",
		"calls area x 24",
		"calls up  25",
	}
	slices.Sort(edges)
	slices.Sort(wantEdges)
	if !slices.Equal(edges, wantEdges) {
		t.Errorf("edges:\n got %s\nwant %s", strings.Join(edges, ", "), strings.Join(wantEdges, ", "))
	}

	binds := map[string]map[string]bool{}
	for _, b := range res.Bindings {
		if binds[b.Name] == nil {
			binds[b.Name] = map[string]bool{}
		}
		binds[b.Name][b.TypeName] = true
	}
	for name, typ := range map[string]string{
		"s": "Area", "m": "Circle", "x": "Circle", "y": "Circle", "z": "Circle", "f": "Formatter",
	} {
		if !binds[name][typ] {
			t.Errorf("binding %s: want %s, got %v", name, typ, binds[name])
		}
	}
	// self/Self are bound to the impl (or trait) type for each body.
	for _, name := range []string{"self", "Self"} {
		if !binds[name]["Circle"] || !binds[name]["Area"] {
			t.Errorf("binding %s: want Circle and Area, got %v", name, binds[name])
		}
	}
}

// rustParser returns a parse func over one registry (compiling the queries is
// the expensive part) closed when the test ends.
func rustParser(t *testing.T) func(src string) Result {
	t.Helper()
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return func(src string) Result {
		t.Helper()
		res, err := r.ParseFile("rust", "src/lib.rs", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
}

// TestParseRustCallShapes pins one call edge (callee + receiver) for every call
// shape, plain and turbofish, written directly and inside a macro's arguments.
// The plain and turbofish forms are separate patterns in rust.scm and once
// drifted apart (crate::f::<T>() emitted nothing). Inside a macro the receiver
// is the token before `.` or `::`, so T::<u8>::f() keeps none there.
func TestParseRustCallShapes(t *testing.T) {
	parseRust := rustParser(t)
	for _, c := range []struct{ call, recv, macroRecv string }{
		{"f()", "", ""},
		{"f::<u8>()", "", ""},
		{"x.f()", "x", "x"},
		{"x.f::<u8>()", "x", "x"},
		{"self.f()", "self", "self"},
		{"T::f()", "T", "T"},
		{"T::f::<u8>()", "T", "T"},
		{"a::T::f()", "T", "T"},
		{"a::T::f::<u8>()", "T", "T"},
		{"T::<u8>::f()", "T", ""},
		{"Self::f()", "Self", "Self"},
		{"self::f()", "", ""},
		{"self::f::<u8>()", "", ""},
		{"super::f()", "", ""},
		{"super::f::<u8>()", "", ""},
		{"crate::f()", "", ""},
		{"crate::f::<u8, Vec<Vec<u8>>>()", "", ""},
		{"f::<g::<u8>>()", "", ""}, // g sits in f's turbofish: not a call
		{"<T as Tr>::f()", "", ""},
		{"<T as Tr>::f::<u8>()", "", ""},
	} {
		for _, form := range []struct{ src, recv string }{
			{"fn caller() { " + c.call + "; }", c.recv},
			{"fn caller() { check!(1, " + c.call + "); }", c.macroRecv},
		} {
			var calls []string
			for _, e := range parseRust(form.src).Edges {
				if e.Kind == "calls" {
					calls = append(calls, e.DstName+" recv="+e.Recv)
				}
			}
			if want := []string{"f recv=" + form.recv}; !slices.Equal(calls, want) {
				t.Errorf("%s: calls %v, want %v", form.src, calls, want)
			}
		}
	}
}

// TestParseRustMacroNonCalls: inside a macro's token trees only an identifier
// followed directly (or after a turbofish) by a parenthesized tree is a call,
// and the macro invocation itself is a reference, not a call.
func TestParseRustMacroNonCalls(t *testing.T) {
	parseRust := rustParser(t)
	for _, args := range []string{"x, (1, 2)", "vec![1]", "a::b", "f::<u8>", "x.y", "[1, 2] (3)", "f::<u8>::g"} {
		src := "fn caller() { check!(" + args + "); }"
		var calls, refs []string
		for _, e := range parseRust(src).Edges {
			switch e.Kind {
			case "calls":
				calls = append(calls, e.DstName)
			case "references":
				refs = append(refs, e.DstName)
			}
		}
		if len(calls) != 0 || !slices.Equal(refs, []string{"check"}) {
			t.Errorf("%s: calls %v (want none), references %v (want [check])", src, calls, refs)
		}
	}
}

// TestParseRustBindingShapes pins a receiver binding x -> Area for every type
// shape, as a parameter AND as a typed let: the two once had separate type lists
// and drifted (let x: &dyn Area<u32> bound nothing).
func TestParseRustBindingShapes(t *testing.T) {
	parseRust := rustParser(t)
	shapes := []string{
		"Area", "&Area", "&mut Area", "Area<u8>", "&Area<u8>", "a::Area", "&a::Area",
		"a::Area<u8>", "&mut a::Area<u8>", "&dyn Area", "&dyn Area<u8>", "&mut dyn Area<u8>",
		"&dyn a::Area", "&dyn a::Area<u8>",
	}
	var srcs []string
	for _, ty := range shapes {
		srcs = append(srcs, "fn f(x: "+ty+") {}", "fn f() { let x: "+ty+" = todo!(); }")
	}
	srcs = append(srcs, "fn f(x: impl Area) {}", "fn f(x: impl Area<u8>) {}", "fn f(x: impl a::Area) {}")
	for _, src := range srcs {
		var got []string
		for _, b := range parseRust(src).Bindings {
			got = append(got, b.Name+":"+b.TypeName)
		}
		if !slices.Equal(got, []string{"x:Area"}) {
			t.Errorf("%s: bindings %v, want [x:Area]", src, got)
		}
	}
}

// TestParseRustMacroScanIsLinear guards security finding F1: an unbalanced
// `a::<a::<…` run inside one macro made every identifier rescan the rest of the
// token tree for its turbofish closer — 32 KB took 4m16s to parse. The matching
// is now one stack pass, so even 4x that input parses in well under a second.
func TestParseRustMacroScanIsLinear(t *testing.T) {
	parseRust := rustParser(t)
	for _, c := range []struct {
		src   string
		calls int
	}{
		{"fn f() { m!(" + strings.Repeat("a::<", 32000) + "); }", 0},
		{"fn f() { m!(" + strings.Repeat("a::<", 16000) + strings.Repeat(">", 16000) + "()); }", 1},
		{"fn f() { m!(" + strings.Repeat("a::<u8>(), ", 16000) + "); }", 16000},
	} {
		start := time.Now()
		res := parseRust(c.src)
		elapsed := time.Since(start)
		calls := 0
		for _, e := range res.Edges {
			if e.Kind == "calls" {
				calls++
			}
		}
		if calls != c.calls || elapsed > 10*time.Second {
			t.Errorf("%d-byte macro: %d calls in %v, want %d calls in well under 10s", len(c.src), calls, elapsed, c.calls)
		}
	}
}
