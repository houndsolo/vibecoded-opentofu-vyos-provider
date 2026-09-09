package provider

import (
	"reflect"
	"testing"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		input string
		path  []string
	}{
		{"set interfaces ethernet eth1 mtu 9189", []string{"interfaces", "ethernet", "eth1", "mtu", "9189"}},
		{" set\tinterfaces dummy dum99 description 'hello world' ", []string{"interfaces", "dummy", "dum99", "description", "hello world"}},
		{`set x "hello world"`, []string{"x", "hello world"}},
		{`set x hello\ world`, []string{"x", "hello world"}},
		{`set x 'a'"b"c`, []string{"x", "abc"}},
		{`set x 'it'\''s fine'`, []string{"x", "it's fine"}},
		{`set x "a\"b\\c\d\$e"`, []string{"x", "a\"b\\c\\d$e"}},
		{`set x '^\d+\s*$'`, []string{"x", `^\d+\s*$`}},
		{`set x ''`, []string{"x", ""}},
		{`set x '$(touch /tmp/no)'`, []string{"x", "$(touch /tmp/no)"}},
		{`set x 2001:db8::1/128`, []string{"x", "2001:db8::1/128"}},
		{`set interfaces ethernet eth1 disable`, []string{"interfaces", "ethernet", "eth1", "disable"}},
		{`delete protocols ospf`, []string{"protocols", "ospf"}},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParseCommand(tc.input)
			if err != nil || !reflect.DeepEqual(got.Path, tc.path) {
				t.Fatalf("got %#v, %v; want %#v", got, err, tc.path)
			}
			round, err := ParseCommand(got.Op + " " + formatPath(got.Path))
			if err != nil || !reflect.DeepEqual(round, got) {
				t.Fatalf("round trip: %#v %v", round, err)
			}
		})
	}
}

func TestInvalidCommands(t *testing.T) {
	for _, input := range []string{"", "set", "delete", "show configuration commands", "run show version", "commit", "SET x y", `set x 'unclosed`, `set x "unclosed`, `set x trailing\`, "set x \x00", `set '' x`, `set x '' y`} {
		if _, err := ParseCommand(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestContradictionsAndCanonicalDuplicates(t *testing.T) {
	for _, pair := range [][]string{
		{"delete protocols ospf", "set protocols ospf area 0 network 10.0.0.0/8"},
		{"delete x y", "set x y"},
		{`delete x 'two words'`, `set x "two words" z`},
	} {
		if _, err := ParseCommands(pair); err == nil {
			t.Errorf("accepted contradiction: %v", pair)
		}
	}
	got, err := ParseCommands([]string{`set x "hello world"`, `set x 'hello world'`, "set protocols", "delete protocols ospf"})
	if err != nil || len(got) != 3 {
		t.Fatalf("valid assertions: %v %v", got, err)
	}
}

func FuzzParserRoundTrip(f *testing.F) {
	for _, value := range []string{"hello world", "'", "\\", "\n", "\"$`", "2001:db8::1/128"} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		c, err := ParseCommand("set x " + formatPath([]string{value}))
		if err != nil {
			return
		}
		if c.Path[1] != value {
			t.Fatalf("round trip changed value")
		}
	})
}
