// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// translatorCase is a message the translator maps, and its direction.
type translatorCase struct {
	msg      godap.Message
	outgoing bool // a request the session sends
}

// translatorCases are the messages the translator maps. TestTranslatorCovers
// checks that every go-dap message type holding a Source is here.
func translatorCases() []translatorCase {
	out := func(m godap.Message) translatorCase { return translatorCase{m, true} }
	in := func(m godap.Message) translatorCase { return translatorCase{m, false} }

	return []translatorCase{
		out(&setBreakpointsRequest{}),
		out(&godap.SetBreakpointsRequest{}),
		out(&godap.BreakpointLocationsRequest{}),
		out(&godap.GotoTargetsRequest{}),
		out(&godap.SourceRequest{}),
		in(&godap.StackTraceResponse{}),
		in(&godap.ScopesResponse{}),
		in(&godap.SetBreakpointsResponse{}),
		in(&godap.SetFunctionBreakpointsResponse{}),
		in(&godap.SetExceptionBreakpointsResponse{}),
		in(&godap.SetDataBreakpointsResponse{}),
		in(&godap.SetInstructionBreakpointsResponse{}),
		in(&godap.BreakpointEvent{}),
		in(&godap.LoadedSourcesResponse{}),
		in(&godap.LoadedSourceEvent{}),
		in(&godap.OutputEvent{}),
		in(&godap.DisassembleResponse{}),
	}
}

// sourceMessageTypes parses go-dap's schema and returns the names of its
// message types (…Request, …Response, …Event) that hold a Source, directly
// or through other types, pointers and slices.
func sourceMessageTypes(t *testing.T) []string {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}", "github.com/google/go-dap").Output()
	if err != nil {
		t.Fatalf("go list go-dap: %v", err)
	}

	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(strings.TrimSpace(string(out)), "schematypes.go"), nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}

	fields := structFieldTypes(file)
	holds := map[string]bool{"Source": true}

	for changed := true; changed; {
		changed = false

		for name, used := range fields {
			if !holds[name] && slices.ContainsFunc(used, func(u string) bool { return holds[u] }) {
				holds[name], changed = true, true
			}
		}
	}

	var names []string

	for name := range holds {
		if strings.HasSuffix(name, "Request") || strings.HasSuffix(name, "Response") || strings.HasSuffix(name, "Event") {
			names = append(names, name)
		}
	}

	slices.Sort(names)

	return names
}

// structFieldTypes maps each struct type of file to the type names its
// fields use.
func structFieldTypes(file *ast.File) map[string][]string {
	fields := map[string][]string{}

	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}

		if st, ok := ts.Type.(*ast.StructType); ok {
			for _, f := range st.Fields.List {
				if name := baseTypeName(f.Type); name != "" {
					fields[ts.Name.Name] = append(fields[ts.Name.Name], name)
				}
			}
		}

		return false
	})

	return fields
}

// baseTypeName is the named type at the bottom of pointers, slices and
// arrays (map values too), or "" for anything else.
func baseTypeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return baseTypeName(x.X)
	case *ast.ArrayType:
		return baseTypeName(x.Elt)
	case *ast.MapType:
		return baseTypeName(x.Value)
	default:
		return ""
	}
}

// A go-dap upgrade that adds a message holding a Source fails here until
// the translator (and translatorCases) handles it.
func TestTranslatorCovers(t *testing.T) {
	t.Parallel()

	var covered []string

	for _, c := range translatorCases() {
		if typ := reflect.TypeOf(c.msg).Elem(); typ.PkgPath() == reflect.TypeFor[godap.Source]().PkgPath() {
			covered = append(covered, typ.Name())
		}
	}

	names := sourceMessageTypes(t)
	if len(names) < 10 {
		t.Fatalf("found only %v: the schema parse is broken", names)
	}

	for _, name := range names {
		if !slices.Contains(covered, name) {
			t.Errorf("go-dap's %s holds a Source the path translator doesn't map", name)
		}
	}
}

// fillSources sets every Source reachable from v (allocating pointers and
// one element of every slice of structs) to path, named "keep", each with
// nested sources like it; depth bounds the recursion.
func fillSources(v reflect.Value, path string, depth int) {
	if depth > 12 {
		return
	}

	if v.Kind() == reflect.Pointer || v.Kind() == reflect.Slice {
		if elem := fillOne(v); elem.IsValid() {
			fillSources(elem, path, depth+1)
		}

		return
	}

	if v.Kind() != reflect.Struct {
		return
	}

	if src, ok := reflect.TypeAssert[*godap.Source](v.Addr()); ok {
		src.Path, src.Name = path, "keep"
		src.Sources = []godap.Source{{Path: path, Name: "keep", Sources: []godap.Source{{Path: path, Name: "keep"}}}}

		return
	}

	for i := range v.NumField() {
		if v.Type().Field(i).IsExported() {
			fillSources(v.Field(i), path, depth+1)
		}
	}
}

// fillOne makes a pointer to a struct, or a slice of structs or pointers,
// hold one element and returns it; anything else returns the zero Value.
func fillOne(v reflect.Value) reflect.Value {
	switch elem := v.Type().Elem().Kind(); {
	case v.Kind() == reflect.Pointer && elem == reflect.Struct:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}

		return v.Elem()
	case v.Kind() == reflect.Slice && (elem == reflect.Struct || elem == reflect.Pointer):
		if v.Len() == 0 {
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		}

		return v.Index(0)
	default:
		return reflect.Value{}
	}
}

// collectSources returns every Source reachable from v.
func collectSources(v reflect.Value, into *[]*godap.Source) {
	switch {
	case v.Kind() == reflect.Pointer && !v.IsNil():
		collectSources(v.Elem(), into)
	case v.Kind() == reflect.Slice:
		for i := range v.Len() {
			collectSources(v.Index(i), into)
		}
	case v.Kind() == reflect.Struct:
		if src, ok := reflect.TypeAssert[*godap.Source](v.Addr()); ok {
			*into = append(*into, src)
		}

		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				collectSources(v.Field(i), into)
			}
		}
	}
}

// translate runs msg through tr in c's direction.
func translate(t *testing.T, tr pathTranslator, msg any, outgoing bool) {
	t.Helper()

	if outgoing {
		req, ok := msg.(godap.RequestMessage)
		if !ok {
			t.Fatalf("%T is not a request", msg)
		}

		tr.Outgoing(req)

		return
	}

	m, ok := msg.(godap.Message)
	if !ok {
		t.Fatalf("%T is not a message", msg)
	}

	tr.Incoming(m)
}

// Every Source of every message maps (nested ones too) in its direction,
// names stay, and unmapped paths and messages without sources pass
// unchanged.
func TestTranslatorMapsEverySource(t *testing.T) {
	t.Parallel()

	host := realTempDir(t)
	tr := pathTranslator{m: mustPathMap(t, api.PathMapping{Remote: "/src", Local: host})}
	hostFile, remoteFile := filepath.Join(host, "a", "b.cs"), "/src/a/b.cs"

	for _, c := range translatorCases() {
		typ := reflect.TypeOf(c.msg).Elem()

		mapped := struct{ from, want string }{remoteFile, hostFile}
		unmapped := "/elsewhere/x.cs"

		if c.outgoing {
			mapped = struct{ from, want string }{hostFile, remoteFile}
			unmapped = filepath.Join(filepath.Dir(host), "elsewhere.cs")
		}

		for _, tc := range []struct{ from, want string }{mapped, {unmapped, unmapped}, {"", ""}} {
			t.Run(typ.Name()+" from "+strconv.Quote(tc.from), func(t *testing.T) {
				t.Parallel()

				v := reflect.New(typ)
				fillSources(v.Elem(), tc.from, 0)
				translate(t, tr, v.Interface(), c.outgoing)

				var got []*godap.Source

				collectSources(v, &got)

				if len(got) < 3 {
					t.Fatalf("only %d sources filled", len(got))
				}

				for _, src := range got {
					if src.Path != tc.want || src.Name != "keep" {
						t.Errorf("source %+v, want path %q, name keep", *src, tc.want)
					}
				}
			})
		}

		// Nil sources, nil arguments and empty lists: nothing to do.
		t.Run(typ.Name()+" empty", func(t *testing.T) {
			t.Parallel()

			translate(t, tr, reflect.New(typ).Interface(), c.outgoing)
		})
	}
}

// A session without a path map has no translator: a nil interface, not a
// typed nil the client would call.
func TestTranslatorNilWithoutMap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		s    *Session
		want bool
	}{
		{"no map", &Session{}, false},
		{"empty map", &Session{pathMap: &PathMap{}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.s.translator() != nil; got != tt.want {
				t.Errorf("translator() != nil is %v, want %v", got, tt.want)
			}
		})
	}
}
