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

// Some models spell the tag with "-ing"; hybrid models emit analysis at the
// start of content with only the closing tag. Both are reasoning, not answer.
func TestSplitThinkingVariants(t *testing.T) {
	think, ans := splitThinking(thinkOpenI + "plan" + thinkCloseI + "answer")
	if think != "plan" || ans != "answer" {
		t.Fatalf("-ing tag: think=%q ans=%q", think, ans)
	}
	think, ans = splitThinking("analysis" + thinkClose + "answer")
	if think != "analysis" || ans != "answer" {
		t.Fatalf("closing-only: think=%q ans=%q", think, ans)
	}
}

func TestHoldThinkPrefix(t *testing.T) {
	cases := map[string]string{
		"answer":      "answer",
		"answer<thi":  "answer",
		"answer</thi": "answer",
		"dangling <":  "dangling ",
	}
	for in, want := range cases {
		if got := holdThinkPrefix(in); got != want {
			t.Fatalf("holdThinkPrefix(%q)=%q want %q", in, got, want)
		}
	}
}
