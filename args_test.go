package main

import (
	"reflect"
	"testing"
)

func TestReadPromptRequiresQuotedPrintPrompt(t *testing.T) {
	if _, err := readPrompt([]string{"review", "main.go"}, true); err == nil {
		t.Fatal("unquoted -p prompt must be rejected")
	}
	got, err := readPrompt([]string{"review main.go"}, true)
	if err != nil || got != "review main.go" {
		t.Fatalf("readPrompt(quoted) = %q, %v", got, err)
	}
	got, err = readPrompt([]string{"just", "a", "prompt"}, false)
	if err != nil || got != "just a prompt" {
		t.Fatalf("readPrompt(headless join) = %q, %v", got, err)
	}
}

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
