package main

import "testing"

func TestSplitThinking(t *testing.T) {
	think, ans := splitThinking("<think>hmm, let me think</think>The answer is 4.")
	if think != "hmm, let me think" || ans != "The answer is 4." {
		t.Fatalf("got think=%q ans=%q", think, ans)
	}
	think, ans = splitThinking("no thinking here")
	if think != "" || ans != "no thinking here" {
		t.Fatalf("got think=%q ans=%q", think, ans)
	}
	think, ans = splitThinking("<think>a</think> and <think>b</think> final")
	if think != "a\nb" || ans != "and  final" {
		t.Fatalf("got think=%q ans=%q", think, ans)
	}
	think, ans = splitThinking("<think>dangling")
	if think != "dangling" || ans != "" {
		t.Fatalf("got think=%q ans=%q", think, ans)
	}
}
