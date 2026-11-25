package designer

// Designer is a Vite bundle: index.html loads an entry script, which imports
// chunks. One chunk, about 18 MB of minified JavaScript, holds the whole app,
// codec included. Running the whole app would need a browser. Instead
// ampiocli parses the chunk and copies out only the top-level declarations
// the codec reaches, the way a linker keeps only the functions a program
// calls. The result is a small program (under 1 MB) that runs in goja, a
// JavaScript interpreter written in Go.

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/js"
)

// chunk is one parsed bundle file.
type chunk struct {
	file    string
	ast     *js.AST
	top     map[*js.Var]bool    // the variables declared at the chunk's top level
	parts   map[string][]part   // top-level declarations, and `window.X = ...` statements, by X
	other   []part              // the rest of the top-level code
	imports map[string]imported // by local name
	exports map[string]string   // exported name → local name
}

// part is a piece of top-level code that can be copied on its own: one
// declarator of a var/let/const (*js.BindingElement), a function or class
// declaration, or a statement installing a global on window (*js.ExprStmt).
type part struct {
	pos  int // order in the chunk
	node js.INode
}

type imported struct{ file, name string }

// sharedImport is the module-federation loader Designer uses for libraries it
// shares with plugins. The codec only needs zustand from it; runtime.js
// provides that.
const sharedImport = "importShared"

func parseChunk(file string, src []byte) (*chunk, error) {
	ast, err := js.Parse(parse.NewInputBytes(src), js.Options{})
	if err != nil {
		return nil, fmt.Errorf("parsing Designer %s: %w", file, err)
	}
	c := &chunk{file: file, ast: ast, top: map[*js.Var]bool{}, parts: map[string][]part{},
		imports: map[string]imported{}, exports: map[string]string{}}
	for _, v := range ast.BlockStmt.Scope.Declared {
		c.top[v] = true
	}
	pos := 0
	add := func(name []byte, node js.INode) {
		c.parts[string(name)] = append(c.parts[string(name)], part{pos, node})
		pos++
	}
	for _, st := range ast.BlockStmt.List {
		switch s := st.(type) {
		case *js.ImportStmt:
			for _, a := range s.List {
				c.imports[string(a.Binding)] = imported{resolve(file, strings.Trim(string(s.Module), `"'`)), cmp.Or(string(a.Name), string(a.Binding))}
			}
		case *js.ExportStmt:
			for _, a := range s.List {
				c.exports[string(a.Binding)] = cmp.Or(string(a.Name), string(a.Binding))
			}
		case *js.VarDecl:
			for i := range s.List {
				decl := &s.List[i]
				bound(decl.Binding, func(v *js.Var) { add(v.Data, decl) })
			}
		case *js.FuncDecl:
			if s.Name != nil {
				add(s.Name.Data, s)
			}
		case *js.ClassDecl:
			if s.Name != nil {
				add(s.Name.Data, s)
			}
		case *js.ExprStmt:
			if name := windowInstall(s); name != nil {
				add(name, s)
				continue
			}
			c.other = append(c.other, part{pos, s})
			pos++
		default:
			c.other = append(c.other, part{pos, s})
			pos++
		}
	}
	return c, nil
}

// bound calls f for every variable a declaration binds, including the ones
// inside destructuring patterns.
func bound(b js.IBinding, f func(*js.Var)) {
	switch b := b.(type) {
	case *js.Var:
		f(b)
	case *js.BindingArray:
		for _, e := range b.List {
			bound(e.Binding, f)
		}
		bound(b.Rest, f)
	case *js.BindingObject:
		for _, item := range b.List {
			bound(item.Value.Binding, f)
		}
		if b.Rest != nil {
			f(b.Rest)
		}
	}
}

// windowInstall returns X for a statement `window.X = ...`.
func windowInstall(s *js.ExprStmt) []byte {
	assign, ok := s.Value.(*js.BinaryExpr)
	if !ok || assign.Op != js.EqToken {
		return nil
	}
	dot, ok := assign.X.(*js.DotExpr)
	if !ok {
		return nil
	}
	obj, ok := dot.X.(*js.Var)
	prop, isName := dot.Y.(js.LiteralExpr)
	if !ok || !isName || string(obj.Data) != "window" || obj.Decl != js.NoDecl {
		return nil
	}
	return prop.Data
}

// uses lists the top-level names n refers to: the chunk's own declarations,
// its imports, and globals it installs on window, read as X or window.X.
// Local variables that happen to share a top-level name are told apart by
// the parser's scope analysis.
func (c *chunk) uses(n js.INode) []string {
	var names []string
	walk(n, func(n js.INode) bool {
		switch n := n.(type) {
		case *js.DotExpr:
			if obj, ok := n.X.(*js.Var); ok && string(obj.Data) == "window" && obj.Decl == js.NoDecl {
				if prop, ok := n.Y.(js.LiteralExpr); ok && c.parts[string(prop.Data)] != nil {
					names = append(names, string(prop.Data))
				}
			}
		case *js.Var:
			v := n
			for v.Link != nil {
				v = v.Link
			}
			name := string(v.Data)
			_, isImport := c.imports[name]
			if c.top[v] || v.Decl == js.NoDecl && (isImport || c.parts[name] != nil) {
				names = append(names, name)
			}
		}
		return true
	})
	return names
}

// slice returns the parts the named roots reach, in chunk order, and the
// local names of the imports they use.
//
// Other top-level code is kept when it changes something kept, as library
// set-up does (Long.fromInt = ..., X.prototype.f = ...), since the browser
// would have run it. The rest (rendering, event wiring, loading React) only
// reads what is kept and is left out.
func (c *chunk) slice(roots []string) (parts []part, imports []string) {
	seen := map[string]bool{}
	keep := func(p part) []string {
		if slices.Contains(parts, p) { // a destructuring declarator binds several names
			return nil
		}
		parts = append(parts, p)
		return c.uses(p.node)
	}
	for queue := slices.Clone(roots); len(queue) > 0; {
		for len(queue) > 0 {
			name := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			if seen[name] {
				continue
			}
			seen[name] = true
			if _, ok := c.imports[name]; ok {
				imports = append(imports, name)
				continue
			}
			for _, p := range c.parts[name] {
				queue = append(queue, keep(p)...)
			}
		}
		for _, p := range c.other {
			if c.changes(p.node, seen) {
				queue = append(queue, keep(p)...)
			}
		}
	}
	slices.SortFunc(parts, func(a, b part) int { return a.pos - b.pos })
	slices.Sort(imports)
	return parts, imports
}

var assignment = map[js.TokenType]bool{
	js.EqToken: true, js.AddEqToken: true, js.SubEqToken: true, js.MulEqToken: true, js.DivEqToken: true,
	js.ModEqToken: true, js.ExpEqToken: true, js.LtLtEqToken: true, js.GtGtEqToken: true, js.GtGtGtEqToken: true,
	js.BitAndEqToken: true, js.BitOrEqToken: true, js.BitXorEqToken: true,
	js.AndEqToken: true, js.OrEqToken: true, js.NullishEqToken: true,
}

// objectSetters are the Object functions that change their first argument.
var objectSetters = map[string]bool{"defineProperty": true, "defineProperties": true, "assign": true, "setPrototypeOf": true}

// changes reports whether n changes one of names: assigns to it or into it
// (X = ..., X.a.b = ..., X[k] += ...), or sets properties on it with
// Object.defineProperty and the like.
func (c *chunk) changes(n js.INode, names map[string]bool) bool {
	found := false
	walk(n, func(n js.INode) bool {
		switch n := n.(type) {
		case *js.BinaryExpr:
			found = assignment[n.Op] && names[c.root(n.X)]
		case *js.CallExpr:
			dot, ok := n.X.(*js.DotExpr)
			if !ok || len(n.Args.List) == 0 {
				break
			}
			obj, isVar := dot.X.(*js.Var)
			prop, isName := dot.Y.(js.LiteralExpr)
			found = isVar && string(obj.Data) == "Object" && obj.Decl == js.NoDecl && isName &&
				objectSetters[string(prop.Data)] && names[c.root(n.Args.List[0].Value)]
		}
		return !found
	})
	return found
}

// root returns the top-level name an expression such as X, X.a.b or X[k]
// starts from, or "" if it starts from something else.
func (c *chunk) root(e js.IExpr) string {
	for {
		switch x := e.(type) {
		case *js.DotExpr:
			e = x.X
		case *js.IndexExpr:
			e = x.X
		case *js.Var:
			if names := c.uses(x); len(names) == 1 {
				return names[0]
			}
			return ""
		default:
			return ""
		}
	}
}

// program is the codec program: a JavaScript function returning a promise of
// the codec's entry points. Each chunk becomes a module function that runs
// once, on first import, like an ES module, and evaluates the parts of the
// chunk the codec needs in their original order.
type program struct {
	src    *source
	chunks map[string]*chunk
	// exports holds, per chunk, the names other chunks (or ampiocli) take
	// from it: key of the module's result → the chunk's top-level name.
	exports map[string]map[string]string
	order   []string // chunks in the order they were reached
}

func (p *program) chunk(file string) (*chunk, error) {
	if c, ok := p.chunks[file]; ok {
		return c, nil
	}
	src, err := p.src.get(file)
	if err != nil {
		return nil, err
	}
	c, err := parseChunk(file, src)
	if err != nil {
		return nil, err
	}
	p.chunks[file] = c
	return c, nil
}

// build returns the program for the names entry exports.
func (p *program) build(entry *chunk, names map[string]string) (string, error) {
	p.exports = map[string]map[string]string{entry.file: names}
	p.order = []string{entry.file}
	// What a chunk must export depends on what its importers keep, which can
	// grow as more is asked of them: repeat until nothing changes.
	for queue := []string{entry.file}; len(queue) > 0; queue = queue[1:] {
		c := p.chunks[queue[0]]
		_, imports, err := p.slice(c)
		if err != nil {
			return "", err
		}
		for _, local := range imports {
			imp := c.imports[local]
			if imp.name == sharedImport {
				continue
			}
			dep, err := p.chunk(imp.file)
			if err != nil {
				return "", err
			}
			inner, ok := dep.exports[imp.name]
			if !ok {
				return "", fmt.Errorf("Designer changed: %s does not export %s", imp.file, imp.name)
			}
			if p.exports[imp.file] == nil {
				p.exports[imp.file] = map[string]string{}
				p.order = append(p.order, imp.file)
			}
			if _, ok := p.exports[imp.file][imp.name]; !ok {
				p.exports[imp.file][imp.name] = inner
				queue = append(queue, imp.file)
			}
		}
	}

	var w strings.Builder
	w.WriteString("() => {\nvar __ampiocli = {};\n")
	for _, file := range p.order {
		if err := p.module(&w, p.chunks[file]); err != nil {
			return "", err
		}
	}
	fmt.Fprintf(&w, "return __ampiocli[%q]();\n}", entry.file)
	return w.String(), nil
}

// slice slices c for the names asked of it.
func (p *program) slice(c *chunk) ([]part, []string, error) {
	roots := slices.Sorted(maps.Values(p.exports[c.file]))
	for _, name := range roots {
		if c.parts[name] == nil {
			return nil, nil, fmt.Errorf("Designer changed: %s has no top-level %s", c.file, name)
		}
	}
	parts, imports := c.slice(roots)
	return parts, imports, nil
}

// module writes the module function of one chunk. Chunk code is module
// code, so it runs in strict mode.
func (p *program) module(w *strings.Builder, c *chunk) error {
	parts, imports, err := p.slice(c)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "__ampiocli[%q] = once(async () => {\n\"use strict\";\n", c.file)
	byFile := map[string][]string{} // file → `"exported": local` bindings
	for _, local := range imports {
		imp := c.imports[local]
		if imp.name == sharedImport {
			fmt.Fprintf(w, "var %s = importShared;\n", local)
			continue
		}
		byFile[imp.file] = append(byFile[imp.file], fmt.Sprintf("%q: %s", imp.name, local))
	}
	for _, file := range slices.Sorted(maps.Keys(byFile)) {
		fmt.Fprintf(w, "var {%s} = await __ampiocli[%q]();\n", strings.Join(byFile[file], ", "), file)
	}
	for _, part := range parts {
		if _, ok := part.node.(*js.BindingElement); ok {
			w.WriteString("var ") // declared one by one; a top-level const or let behaves the same
			part.node.JS(w)
			w.WriteString(";")
		} else {
			part.node.JS(w)
		}
		w.WriteString("\n")
	}
	w.WriteString("return {")
	exports := p.exports[c.file]
	for _, key := range slices.Sorted(maps.Keys(exports)) {
		fmt.Fprintf(w, "get %q() { return %s }, ", key, exports[key])
	}
	w.WriteString("};\n});\n")
	return nil
}

// codecNames finds the minified names of the codec's entry points. They
// change with every Designer release, so they are recognised by what the
// code does, not by name:
//
//	save(mac, rules) {               the function that saves a module's logic:
//	  const bytes = ENCODE(...)        first an encode,
//	  REGISTRY.updateConditionsArrByMacUniq(...)
//	  return DECODE(...)               and it returns a decode
//	}
//	INIT_REGISTRY = () => { REGISTRY = new ... }
//	INIT = () => { ...; INIT_REGISTRY(); ... }   Designer's codec start-up
//	ADD_DEVICE = (row) => { REGISTRY.addNewDevice(...); ...parseAndAddGroupsFromDev(...) }
//
// Each must match exactly once; anything else is an error, never a guess.
func (c *chunk) codecNames() (map[string]string, error) {
	type save struct{ encode, decode, registry string }
	var saves []save
	walk(c.ast, func(n js.INode) bool {
		_, body, ok := function(n)
		if !ok || len(body.List) < 2 {
			return true
		}
		first, _ := body.List[0].(*js.VarDecl)
		last, _ := body.List[len(body.List)-1].(*js.ReturnStmt)
		if first == nil || last == nil {
			return true
		}
		encode, decode := c.calledName(first.List[0].Default), c.calledName(last.Value)
		var registry string
		contains(body, func(call *js.CallExpr) bool {
			registry = c.topName(method(call, "updateConditionsArrByMacUniq"))
			return registry != ""
		})
		if encode != "" && decode != "" && registry != "" {
			saves = append(saves, save{encode, decode, registry})
		}
		return true
	})
	if len(saves) != 1 {
		return nil, fmt.Errorf("Designer changed: expected one logic save function, found %d", len(saves))
	}
	s := saves[0]

	initRegistry, err := c.onlyFunction("registry start-up", 0, func(body *js.BlockStmt) bool {
		return assigns(body, func(target, value js.IExpr) bool {
			_, isNew := value.(*js.NewExpr)
			return isNew && c.topName(target) == s.registry
		})
	})
	if err != nil {
		return nil, err
	}
	init, err := c.onlyFunction("codec start-up", 0, func(body *js.BlockStmt) bool {
		return contains(body, func(call *js.CallExpr) bool { return c.calledName(call) == initRegistry })
	})
	if err != nil {
		return nil, err
	}
	addDevice, err := c.onlyFunction("device registration", 1, func(body *js.BlockStmt) bool {
		return contains(body, func(call *js.CallExpr) bool { return c.topName(method(call, "addNewDevice")) == s.registry }) &&
			contains(body, func(call *js.CallExpr) bool { return method(call, "parseAndAddGroupsFromDev") != nil })
	})
	if err != nil {
		return nil, err
	}
	return map[string]string{"encode": s.encode, "decode": s.decode, "registry": s.registry, "init": init, "addDevice": addDevice}, nil
}

// onlyFunction returns the one top-level function with the given number of
// parameters whose body matches.
func (c *chunk) onlyFunction(what string, params int, match func(*js.BlockStmt) bool) (string, error) {
	var found []string
	for name, list := range c.parts {
		for _, p := range list {
			node := p.node
			if decl, ok := node.(*js.BindingElement); ok {
				node = decl.Default
			}
			if ps, body, ok := function(node); ok && len(ps.List) == params && ps.Rest == nil && match(body) {
				found = append(found, name)
			}
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("Designer changed: expected one %s function, found %d", what, len(found))
	}
	return found[0], nil
}

// topName returns the name of e if it is a top-level variable of the chunk.
func (c *chunk) topName(e js.IExpr) string {
	v, ok := e.(*js.Var)
	if !ok {
		return ""
	}
	for v.Link != nil {
		v = v.Link
	}
	if !c.top[v] {
		return ""
	}
	return string(v.Data)
}

// calledName returns NAME for a call NAME(...) of a top-level function.
func (c *chunk) calledName(e js.IExpr) string {
	if call, ok := e.(*js.CallExpr); ok {
		return c.topName(call.X)
	}
	return ""
}

// method returns the receiver of a call receiver.name(...), or nil.
func method(call *js.CallExpr, name string) js.IExpr {
	if dot, ok := call.X.(*js.DotExpr); ok {
		if prop, ok := dot.Y.(js.LiteralExpr); ok && string(prop.Data) == name {
			return dot.X
		}
	}
	return nil
}

// function returns the parameters and body of a function node.
func function(n js.INode) (*js.Params, *js.BlockStmt, bool) {
	switch f := n.(type) {
	case *js.ArrowFunc:
		return &f.Params, &f.Body, true
	case *js.FuncDecl:
		return &f.Params, &f.Body, true
	case *js.MethodDecl:
		return &f.Params, &f.Body, true
	}
	return nil, nil, false
}

// contains reports whether n holds a call that match accepts.
func contains(n js.INode, match func(*js.CallExpr) bool) bool {
	found := false
	walk(n, func(n js.INode) bool {
		if call, ok := n.(*js.CallExpr); ok && !found && match(call) {
			found = true
		}
		return !found
	})
	return found
}

// assigns reports whether n holds an assignment `target = value` that match accepts.
func assigns(n js.INode, match func(target, value js.IExpr) bool) bool {
	found := false
	walk(n, func(n js.INode) bool {
		if b, ok := n.(*js.BinaryExpr); ok && !found && b.Op == js.EqToken && match(b.X, b.Y) {
			found = true
		}
		return !found
	})
	return found
}

// walk visits n and its descendants depth-first while visit returns true.
func walk(n js.INode, visit func(js.INode) bool) { js.Walk(visitor(visit), n) }

type visitor func(js.INode) bool

func (v visitor) Enter(n js.INode) js.IVisitor {
	if v(n) {
		return v
	}
	return nil
}

func (visitor) Exit(js.INode) {}
