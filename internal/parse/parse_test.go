package parse

import (
	"slices"
	"testing"
)

func names(syms []Symbol, kind string) []string {
	var out []string
	for _, s := range syms {
		if s.Kind == kind {
			out = append(out, s.Name)
		}
	}
	return out
}

func TestParseGo(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte(`package p

import "fmt"

type Greeter struct{}

func (g Greeter) Hello(name string) string {
	return fmt.Sprintf("hi %s", name)
}

func Run() {
	g := Greeter{}
	g.Hello("world")
}
`)
	res, err := r.ParseFile("go", "p/greet.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names(res.Symbols, "function"), "Run") {
		t.Errorf("missing function Run; functions=%v", names(res.Symbols, "function"))
	}
	if !slices.Contains(names(res.Symbols, "method"), "Hello") {
		t.Errorf("missing method Hello; methods=%v", names(res.Symbols, "method"))
	}
	if !slices.Contains(names(res.Symbols, "type"), "Greeter") {
		t.Errorf("missing type Greeter; types=%v", names(res.Symbols, "type"))
	}
	var calls, imports []string
	for _, e := range res.Edges {
		switch e.Kind {
		case "calls":
			calls = append(calls, e.DstName)
		case "imports":
			imports = append(imports, e.DstName)
		}
	}
	if !slices.Contains(calls, "Sprintf") || !slices.Contains(calls, "Hello") {
		t.Errorf("calls = %v, want Sprintf and Hello", calls)
	}
	if !slices.Contains(imports, "fmt") {
		t.Errorf("imports = %v, want fmt", imports)
	}
	for _, s := range res.Symbols {
		if s.EndByte <= s.StartByte {
			t.Errorf("symbol %q has empty byte range", s.Name)
		}
	}
}

func TestParseCSharp(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte(`namespace App {
  class Service {
    public void Run() { Helper.Do(); }
  }
}`)
	res, err := r.ParseFile("csharp", "Service.cs", src)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names(res.Symbols, "class"), "Service") {
		t.Errorf("missing class Service; classes=%v", names(res.Symbols, "class"))
	}
	if !slices.Contains(names(res.Symbols, "method"), "Run") {
		t.Errorf("missing method Run; methods=%v", names(res.Symbols, "method"))
	}
	var calls []string
	for _, e := range res.Edges {
		if e.Kind == "calls" {
			calls = append(calls, e.DstName)
		}
	}
	if !slices.Contains(calls, "Do") {
		t.Errorf("calls = %v, want Do", calls)
	}
}

func TestParseTypeScript(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, lang := range []string{"typescript", "tsx"} {
		src := []byte(`import { x } from "./x";
class Widget { render() { draw(); } }
function mount() { const w = new Widget(); w.render(); }`)
		res, err := r.ParseFile(lang, "w."+lang, src)
		if err != nil {
			t.Fatalf("%s: %v", lang, err)
		}
		if !slices.Contains(names(res.Symbols, "class"), "Widget") {
			t.Errorf("%s: missing class Widget; classes=%v", lang, names(res.Symbols, "class"))
		}
		if !slices.Contains(names(res.Symbols, "function"), "mount") {
			t.Errorf("%s: missing function mount", lang)
		}
		if !slices.Contains(names(res.Symbols, "method"), "render") {
			t.Errorf("%s: missing method render", lang)
		}
	}
}

func TestParseJavaScript(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte(`import React from "react";
class Box { draw() {} }
function App() { render(); return <div/>; }`)
	res, err := r.ParseFile("javascript", "app.jsx", src)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names(res.Symbols, "function"), "App") {
		t.Errorf("missing function App; functions=%v", names(res.Symbols, "function"))
	}
	if !slices.Contains(names(res.Symbols, "class"), "Box") {
		t.Errorf("missing class Box")
	}
	var calls []string
	for _, e := range res.Edges {
		if e.Kind == "calls" {
			calls = append(calls, e.DstName)
		}
	}
	if !slices.Contains(calls, "render") {
		t.Errorf("calls = %v, want render", calls)
	}
}

func TestParseCSharpModernConstructs(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Modern .NET (C# 10+/.NET 6+): file-scoped namespace, records (class & struct),
	// enum + members, delegate, and a class with a constructor and a property.
	src := []byte(`namespace App.Domain;

public enum AppStatus { Active, Stopped }

public sealed record AppDto(System.Guid Id, string Name);

public readonly record struct NetReading(long Rx, long Tx);

public delegate void Notify(string msg);

public class Service
{
    public string Name { get; set; }

    public Service(int count) { }

    public void Run() { }
}
`)
	res, err := r.ParseFile("csharp", "App/Service.cs", src)
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		kind, name string
	}{
		{"namespace", "App.Domain"},
		{"enum", "AppStatus"},
		{"record", "AppDto"},
		{"record", "NetReading"},
		{"delegate", "Notify"},
		{"property", "Name"},
		{"constructor", "Service"},
		{"enum_member", "Active"},
		{"enum_member", "Stopped"},
	}
	for _, c := range checks {
		if !slices.Contains(names(res.Symbols, c.kind), c.name) {
			t.Errorf("missing %s %q; %ss=%v", c.kind, c.name, c.kind, names(res.Symbols, c.kind))
		}
	}
}

func TestParseTypeScriptModernConstructs(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte(`namespace Outer { export const x = 1; }
export enum Color { Red, Green }
export type ID = string;
export type Point = { x: number };
export abstract class Base { abstract run(): void; }
export const useThing = () => 1;
export const Comp: React.FC = () => null;
export const legacy = function () {};
export function classic() {}
`)
	// tsx reuses the typescript query, so both dialects must extract the same set.
	for _, lang := range []string{"typescript", "tsx"} {
		res, err := r.ParseFile(lang, "m."+lang, src)
		if err != nil {
			t.Fatalf("%s: %v", lang, err)
		}
		checks := []struct{ kind, name string }{
			{"namespace", "Outer"},
			{"enum", "Color"},
			{"type", "ID"},
			{"type", "Point"},
			{"class", "Base"},        // abstract_class_declaration
			{"function", "useThing"}, // arrow const
			{"function", "Comp"},     // typed arrow const
			{"function", "legacy"},   // function-expression const
			{"function", "classic"},  // plain function declaration
		}
		for _, c := range checks {
			if !slices.Contains(names(res.Symbols, c.kind), c.name) {
				t.Errorf("%s: missing %s %q; %ss=%v", lang, c.kind, c.name, c.kind, names(res.Symbols, c.kind))
			}
		}
	}
}

func TestParseJavaScriptModernConstructs(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte(`function* gen() { yield 1; }
const useThing = () => {};
export const legacy = function () {};
class Box { draw() {} }
`)
	res, err := r.ParseFile("javascript", "m.js", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ kind, name string }{
		{"function", "gen"},      // generator_function_declaration
		{"function", "useThing"}, // arrow const
		{"function", "legacy"},   // function-expression const
		{"class", "Box"},
	} {
		if !slices.Contains(names(res.Symbols, c.kind), c.name) {
			t.Errorf("missing %s %q; %ss=%v", c.kind, c.name, c.kind, names(res.Symbols, c.kind))
		}
	}
}

func edgesOfKind(es []Edge, kind string) []string {
	var out []string
	for _, e := range es {
		if e.Kind == kind {
			out = append(out, e.DstName)
		}
	}
	return out
}

func TestParseCSharpInheritance(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte("namespace A {\n  interface IFoo {}\n  class Base {}\n  class Svc : Base, IFoo {}\n}")
	res, err := r.ParseFile("csharp", "S.cs", src)
	if err != nil {
		t.Fatal(err)
	}
	// C# base_list does not syntactically distinguish a base class from an
	// interface; capture all base types as "extends".
	ext := edgesOfKind(res.Edges, "extends")
	if !slices.Contains(ext, "Base") || !slices.Contains(ext, "IFoo") {
		t.Errorf("csharp extends = %v, want Base and IFoo", ext)
	}
}

func TestParseTypeScriptInheritance(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte("class Base {}\ninterface IFoo {}\nclass Svc extends Base implements IFoo {}")
	res, err := r.ParseFile("typescript", "s.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(edgesOfKind(res.Edges, "extends"), "Base") {
		t.Errorf("ts extends = %v, want Base", edgesOfKind(res.Edges, "extends"))
	}
	if !slices.Contains(edgesOfKind(res.Edges, "implements"), "IFoo") {
		t.Errorf("ts implements = %v, want IFoo", edgesOfKind(res.Edges, "implements"))
	}
}

func TestParseJavaScriptInheritance(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte("class Base {}\nclass Svc extends Base {}")
	res, err := r.ParseFile("javascript", "s.js", src)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(edgesOfKind(res.Edges, "extends"), "Base") {
		t.Errorf("js extends = %v, want Base", edgesOfKind(res.Edges, "extends"))
	}
}
