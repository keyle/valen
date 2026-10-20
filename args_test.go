package main

import (
	"reflect"
	"testing"
)

func TestReorderFlags(t *testing.T) {
	values := map[string]bool{"config": true, "dir": true, "dvlog": true, "f": true, "file": true}
	cases := []struct {
		in, want []string
	}{
		{[]string{"-p", "review main.go", "-v", "-vv"}, []string{"-p", "-v", "-vv", "review main.go"}},
		{[]string{"-f", "a b.txt", "prompt", "-v"}, []string{"-f", "a b.txt", "-v", "prompt"}},
		{[]string{"prompt", "-config=x.toml"}, []string{"-config=x.toml", "prompt"}},
		{[]string{"prompt", "--", "-v"}, []string{"prompt", "-v"}},
		{[]string{"just", "a", "prompt"}, []string{"just", "a", "prompt"}},
	}
	for _, c := range cases {
		if got := reorderFlags(c.in, values); !reflect.DeepEqual(got, c.want) {
			t.Errorf("reorderFlags(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
