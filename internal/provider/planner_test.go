package provider

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
)

// fixtureAST builds the public VyOS json_ast shape. It is only a fixture
// encoder; it does not share planning or deletion code with the implementation.
func fixtureAST(name string, value any) any {
	values := []string{}
	children := []any{}
	leaf := false
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			children = append(children, fixtureAST(k, v[k]))
		}
	case string:
		values = append(values, v)
		leaf = true
	case []string:
		values = v
		leaf = true
	case nil:
		leaf = true
	default:
		panic("unsupported test fixture")
	}
	return map[string]any{"name": name, "data": map[string]any{"values": values, "comment": nil, "tag": false, "leaf": leaf}, "children": children}
}

func fixtureCommands(path []string, value any) string {
	switch v := value.(type) {
	case map[string]any:
		if len(v) == 0 && len(path) > 0 {
			return "set " + formatPath(path) + "\n"
		}
		var text string
		for k, x := range v {
			text += fixtureCommands(child(path, k), x)
		}
		return text
	case string:
		return "set " + formatPath(child(path, v)) + "\n"
	case []string:
		var text string
		for _, v := range v {
			text += "set " + formatPath(child(path, v)) + "\n"
		}
		return text
	case nil:
		return "set " + formatPath(path) + "\n"
	default:
		panic("unsupported fixture")
	}
}

func TestActiveSnapshotVerification(t *testing.T) {
	tree := dummy(map[string]any{"description": "hello\nworld", "address": []string{"192.0.2.1/32", "2001:db8::1/128"}})
	s := snapshotFixture(t, tree)
	text := fixtureCommands(nil, tree)
	if err := s.VerifyActive(text); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", text + "set protocols ospf\n", "set x 'unfinished\n", text + "deactivate interfaces dummy dum99\n", text + "comment interfaces 'unmanaged'\n"} {
		if err := s.VerifyActive(invalid); err == nil {
			t.Fatalf("accepted incomplete or unsupported active export: %q", invalid)
		}
	}
	comment := "keep this"
	path := []string{"interfaces", "dummy", "dum99"}
	s.nodes[pathKey(path)].Data.Comment = &comment
	s.comments = append(s.comments, path)
	if err := s.VerifyActive(text); err == nil {
		t.Fatal("missing active comment accepted")
	}
	if err := s.VerifyActive(text + "comment interfaces dummy dum99 'keep this'\n"); err != nil {
		t.Fatal(err)
	}
}
func snapshotFixture(t testing.TB, tree map[string]any) *Snapshot {
	t.Helper()
	data, err := json.Marshal(fixtureAST("", tree))
	if err != nil {
		t.Fatal(err)
	}
	s, err := DecodeSnapshot(data)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func commandsFixture(t testing.TB, raw []string) []Command {
	t.Helper()
	c, err := ParseCommands(raw)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func opStrings(ops []Command) []string {
	result := []string{}
	for _, c := range ops {
		result = append(result, c.Op+" "+formatPath(c.Path))
	}
	return result
}
func dummy(values map[string]any) map[string]any {
	return map[string]any{"interfaces": map[string]any{"dummy": map[string]any{"dum99": values}}}
}

func TestBuildBatch(t *testing.T) {
	tests := []struct {
		name           string
		running        map[string]any
		old, want, ops []string
	}{
		{"create missing", map[string]any{}, nil, []string{"set interfaces dummy dum99 mtu 1400", "delete protocols ospf"}, []string{"set interfaces dummy dum99 mtu 1400"}},
		{"create satisfied", dummy(map[string]any{"mtu": "1400"}), nil, []string{"set interfaces dummy dum99 mtu 1400"}, []string{}},
		{"scalar replacement", dummy(map[string]any{"mtu": "1400"}), []string{"set interfaces dummy dum99 mtu 1400"}, []string{"set interfaces dummy dum99 mtu 1450"}, []string{"delete interfaces dummy dum99 mtu", "set interfaces dummy dum99 mtu 1450"}},
		{"multi value removal", dummy(map[string]any{"address": []string{"192.0.2.1/32", "192.0.2.2/32"}}), []string{"set interfaces dummy dum99 address 192.0.2.1/32", "set interfaces dummy dum99 address 192.0.2.2/32"}, []string{"set interfaces dummy dum99 address 192.0.2.2/32"}, []string{"delete interfaces dummy dum99 address 192.0.2.1/32"}},
		{"unmanaged address", dummy(map[string]any{"address": []string{"192.0.2.1/32", "192.0.2.2/32"}}), []string{"set interfaces dummy dum99 address 192.0.2.1/32"}, nil, []string{"delete interfaces dummy dum99 address 192.0.2.1/32"}},
		{"singleton multi value", dummy(map[string]any{"address": "192.0.2.1/32", "description": "KEEP"}), []string{"set interfaces dummy dum99 address 192.0.2.1/32"}, nil, []string{"delete interfaces dummy dum99 address"}},
		{"destroy drifted scalar preserves current", dummy(map[string]any{"mtu": "1420"}), []string{"set interfaces dummy dum99 mtu 1400"}, nil, []string{}},
		{"update drifted scalar", dummy(map[string]any{"mtu": "1420"}), []string{"set interfaces dummy dum99 mtu 1400"}, []string{"set interfaces dummy dum99 mtu 1450"}, []string{"set interfaces dummy dum99 mtu 1450"}},
		{"explicit delete drift", map[string]any{"protocols": map[string]any{"ospf": map[string]any{"area": map[string]any{"0": map[string]any{"network": "10.0.0.0/8"}}}}}, nil, []string{"delete protocols ospf"}, []string{"delete protocols ospf"}},
		{"destroy never recreates assertion", map[string]any{}, []string{"delete protocols ospf"}, nil, []string{}},
		{"parent set does not own descendants", dummy(map[string]any{"mtu": "1400", "address": "192.0.2.1/32"}), []string{"set interfaces dummy dum99"}, nil, []string{}},
		{"retained empty parent", dummy(map[string]any{"mtu": "1400"}), []string{"set interfaces dummy dum99 mtu 1400", "set interfaces dummy dum99"}, []string{"set interfaces dummy dum99"}, []string{"delete interfaces dummy dum99 mtu"}},
		{"different quoting", dummy(map[string]any{"description": "hello world"}), []string{`set interfaces dummy dum99 description 'hello world'`}, []string{`set interfaces dummy dum99 description "hello world"`}, []string{}},
		{"valueless flag", dummy(map[string]any{"disable": nil, "description": "KEEP"}), []string{"set interfaces dummy dum99 disable"}, nil, []string{"delete interfaces dummy dum99 disable"}},
		{"explicit value delete retains sibling", dummy(map[string]any{"address": []string{"a", "b"}}), nil, []string{"delete interfaces dummy dum99 address a", "set interfaces dummy dum99 address b"}, []string{"delete interfaces dummy dum99 address a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			batch, err := BuildBatch(snapshotFixture(t, tc.running), commandsFixture(t, tc.old), commandsFixture(t, tc.want))
			if err != nil {
				t.Fatal(err)
			}
			if got := opStrings(batch.Operations); !reflect.DeepEqual(got, tc.ops) {
				t.Fatalf("got %v; want %v", got, tc.ops)
			}
		})
	}
}

func TestSafeParentPruning(t *testing.T) {
	rule := map[string]any{"action": "permit", "regex": "^$"}
	old := commandsFixture(t, []string{"set policy as-path-list TEST rule 10 action permit", "set policy as-path-list TEST rule 10 regex '^$'"})
	for _, full := range []bool{false, true} {
		rules := map[string]any{"10": rule}
		want := "delete policy"
		if !full {
			rules["20"] = map[string]any{"action": "deny", "regex": ".*"}
			want = "delete policy as-path-list TEST rule 10"
		}
		tree := map[string]any{"policy": map[string]any{"as-path-list": map[string]any{"TEST": map[string]any{"rule": rules}}}}
		batch, err := BuildBatch(snapshotFixture(t, tree), old, nil)
		if err != nil || !reflect.DeepEqual(opStrings(batch.Operations), []string{want}) {
			t.Fatalf("full=%v: %v %v", full, batch, err)
		}
	}
}

func TestUnmanagedSiblingProtection(t *testing.T) {
	tree := map[string]any{"policy": map[string]any{"route-map": map[string]any{"TEST": map[string]any{"rule": map[string]any{"10": map[string]any{
		"action": "permit", "match": map[string]any{"ip": map[string]any{"address": map[string]any{"prefix-list": "KEEP"}}},
	}}}}}}
	batch, err := BuildBatch(snapshotFixture(t, tree), commandsFixture(t, []string{"set policy route-map TEST rule 10 action permit"}), nil)
	if err != nil || !reflect.DeepEqual(opStrings(batch.Operations), []string{"delete policy route-map TEST rule 10 action"}) {
		t.Fatalf("%v %v", batch, err)
	}
}

func TestRedundantDeletes(t *testing.T) {
	input := commandsFixture(t, []string{"delete policy route-map A", "delete policy route-map A rule 1", "delete policy", "delete interfaces dummy x", "delete interfaces dummy x address a", "delete interfaces dummy xyz"})
	want := []string{"delete interfaces dummy x", "delete interfaces dummy xyz", "delete policy"}
	if got := opStrings(minimizeDeletes(input)); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestSnapshotAndComments(t *testing.T) {
	s := snapshotFixture(t, dummy(map[string]any{"description": "hello world", "disable": nil}))
	if !s.Exists([]string{"interfaces", "dummy", "dum99", "description", "hello world"}) || s.Exists([]string{"interfaces", "dummy", "dum99", "description", "hello"}) {
		t.Fatal("value boundary lost")
	}
	comment := "unmanaged comment"
	n := s.nodes[pathKey([]string{"interfaces", "dummy", "dum99"})]
	n.Data.Comment = &comment
	s.comments = append(s.comments, []string{"interfaces", "dummy", "dum99"})
	batch, err := BuildBatch(s, commandsFixture(t, []string{"set interfaces dummy dum99 description 'hello world'", "set interfaces dummy dum99 disable"}), nil)
	if err != nil || len(batch.Operations) != 2 {
		t.Fatalf("comment container pruned: %v %v", batch, err)
	}
	for _, raw := range []string{`{}`, `null`, `{"name":"","data":{},"children":[]}`, `{"name":"","data":{"values":[],"comment":null,"tag":false,"leaf":false},"children":[],"unexpected":true}`} {
		if _, err := DecodeSnapshot([]byte(raw)); err == nil {
			t.Errorf("accepted unsafe AST %s", raw)
		}
	}
}

func BenchmarkLargeBatch(b *testing.B) {
	nodes := map[string]any{}
	var old, desired []string
	for i := 0; i < 2000; i++ {
		name := fmt.Sprintf("dum%d", i)
		nodes[name] = map[string]any{"mtu": "1400", "address": []string{"192.0.2.1/32", "192.0.2.2/32"}}
		old = append(old, "set interfaces dummy "+name+" mtu 1400")
		desired = append(desired, "set interfaces dummy "+name+" mtu 1450")
	}
	s := snapshotFixture(b, map[string]any{"interfaces": map[string]any{"dummy": nodes}})
	o, d := commandsFixture(b, old), commandsFixture(b, desired)
	b.ResetTimer()
	for b.Loop() {
		if _, err := BuildBatch(s, o, d); err != nil {
			b.Fatal(err)
		}
	}
}

func TestPruningOwnershipInvariant(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for trial := 0; trial < 100; trial++ {
		nodes := map[string]any{}
		var old, desired []string
		removable := map[string]bool{}
		for n := 0; n < 8; n++ {
			name := fmt.Sprintf("dum%d", n)
			nodes[name] = map[string]any{"mtu": "1400", "address": []string{"192.0.2.1/32", "192.0.2.2/32"}}
			for _, suffix := range []string{"mtu 1400", "address 192.0.2.1/32", "address 192.0.2.2/32"} {
				raw := "set interfaces dummy " + name + " " + suffix
				c, _ := ParseCommand(raw)
				if rng.IntN(2) == 0 {
					continue
				} // unmanaged terminal
				old = append(old, raw)
				if rng.IntN(2) == 0 {
					desired = append(desired, raw)
				} else {
					removable[pathKey(c.Path)] = true
				}
			}
		}
		s := snapshotFixture(t, map[string]any{"interfaces": map[string]any{"dummy": nodes}})
		batch, err := BuildBatch(s, commandsFixture(t, old), commandsFixture(t, desired))
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range batch.Operations {
			if op.Op != "delete" {
				continue
			}
			for key, terminal := range s.terminals {
				if hasPrefix(terminal, op.Path) && !removable[key] {
					t.Fatalf("trial %d deletes unowned or retained terminal %v with %v", trial, terminal, op)
				}
			}
		}
	}
}

func TestCommentRemovalRequiresExplicitAuthorization(t *testing.T) {
	s := snapshotFixture(t, dummy(map[string]any{"mtu": "1400"}))
	s.comments = append(s.comments, []string{"interfaces", "dummy", "dum99", "mtu"})
	owned := commandsFixture(t, []string{"set interfaces dummy dum99 mtu 1400"})
	if _, err := BuildBatch(s, owned, nil); err == nil {
		t.Fatal("removed an unmanaged comment")
	}
	batch, err := BuildBatch(s, owned, commandsFixture(t, []string{"delete interfaces dummy dum99 mtu"}))
	if err != nil || !reflect.DeepEqual(opStrings(batch.Operations), []string{"delete interfaces dummy dum99 mtu"}) {
		t.Fatalf("explicit authorization failed or expanded: %v %v", batch, err)
	}
}
