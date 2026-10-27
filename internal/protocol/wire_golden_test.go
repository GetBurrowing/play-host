package protocol

import (
	"encoding/json"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var updateWire = flag.Bool("update-wire", false, "rewrite testdata/wire_golden.json (a protocol change: needs core's protocol.ex to change too)")

// wireGolden pins the canvas protocol: every core<->host event name with its
// payload key set (per direction), and the browser message types the input
// binding accepts. Task 01a17b51 constraint (b): capture work changes none of it.
type wireGolden struct {
	Events       map[string]map[string][]string `json:"events"`
	BrowserTypes []string                       `json:"browser_types"`
}

// payloads maps each event to the payload struct per direction.
var payloads = map[string]map[string]reflect.Type{
	EventSessionStart:  {"core_to_host": reflect.TypeOf(SessionStart{})},
	EventSessionStop:   {"core_to_host": reflect.TypeOf(SessionStop{})},
	EventSignal:        {"core_to_host": reflect.TypeOf(Signal{}), "host_to_core": reflect.TypeOf(Signal{})},
	EventSessionStatus: {"host_to_core": reflect.TypeOf(Status{}), "host_to_core_resume": reflect.TypeOf(ResumeStatus{})},
	EventPeerOpen:      {"core_to_host": reflect.TypeOf(PeerRef{})},
	EventPeerClose:     {"core_to_host": reflect.TypeOf(PeerRef{})},
	EventSlots:         {"core_to_host": reflect.TypeOf(Slots{})},
	EventSlotActivity:  {"host_to_core": reflect.TypeOf(SlotActivity{})},
}

// jsonKeys is the sorted JSON key set of a struct, embedded structs flattened.
func jsonKeys(t reflect.Type) []string {
	var keys []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if f.Anonymous && name == "" {
			keys = append(keys, jsonKeys(f.Type)...)
			continue
		}
		if name == "-" || !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return keys
}

// declaredEvents reads the Event* constants out of protocol.go, so an event
// added without a golden entry is caught.
func declaredEvents(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "protocol.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, sp := range g.Specs {
			vs := sp.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if strings.HasPrefix(n.Name, "Event") && len(vs.Values) > i {
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok {
						v, _ := strconv.Unquote(lit.Value)
						out = append(out, v)
					}
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// browserTypes reads the message types Binding.handle switches on, from the
// source, so a new browser t cannot be added without touching the golden.
func browserTypes(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "input", "binding.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "handle" {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					v, _ := strconv.Unquote(lit.Value)
					set[v] = true
				}
			}
			return true
		})
	}
	var out []string
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func currentWire(t *testing.T) wireGolden {
	t.Helper()
	g := wireGolden{Events: map[string]map[string][]string{}, BrowserTypes: browserTypes(t)}
	for ev, dirs := range payloads {
		g.Events[ev] = map[string][]string{}
		for dir, typ := range dirs {
			g.Events[ev][dir] = jsonKeys(typ)
		}
	}
	return g
}

func TestWireGolden(t *testing.T) {
	// Every declared event has a payload entry here (and so a golden entry).
	for _, ev := range declaredEvents(t) {
		if _, ok := payloads[ev]; !ok {
			t.Errorf("event %q is declared in protocol.go but pinned nowhere", ev)
		}
	}
	got := currentWire(t)
	path := filepath.Join("testdata", "wire_golden.json")
	if *updateWire {
		b, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want wireGolden
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.MarshalIndent(got, "", "  ")
		wb, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("the canvas protocol changed (core's protocol.ex must change with it).\ngot:\n%s\nwant:\n%s", gb, wb)
	}
}
