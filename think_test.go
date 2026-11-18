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

// An answer discussing think tags writes them in backticks: those are
// literal mentions, so the answer after them must survive. Before the
// code-aware split, the first literal opening tag cropped the answer at
// the backtick before it and dropped the conclusion into thinking.
func TestSplitThinkingIgnoresCodeMentions(t *testing.T) {
	in := "It only used pendingThinking plus any `<think>` blocks, discarding the preamble."
	think, ans := splitThinking(in)
	if think != "" || ans != in {
		t.Fatalf("inline mention: think=%q ans=%q", think, ans)
	}
	think, ans = splitThinking("Use `<think>` for reasoning. <think>real</think>Answer.")
	if think != "real" || ans != "Use `<think>` for reasoning. Answer." {
		t.Fatalf("real tag after code: think=%q ans=%q", think, ans)
	}
	think, ans = splitThinking("```\n<think>example</think>\n```\nAnswer")
	if think != "" || ans != "```\n<think>example</think>\n```\nAnswer" {
		t.Fatalf("fenced mention: think=%q ans=%q", think, ans)
	}
	think, ans = splitThinking("analysis `</think>` answer")
	if think != "" || ans != "analysis `</think>` answer" {
		t.Fatalf("closing mention: think=%q ans=%q", think, ans)
	}
	// A literal close inside a real think block does not end it early.
	think, ans = splitThinking("<think>use `</think>` here</think>Answer")
	if think != "use `</think>` here" || ans != "Answer" {
		t.Fatalf("close in code inside think: think=%q ans=%q", think, ans)
	}
	// An unclosed inline span ends at a blank line, so a real block after
	// the paragraph still splits.
	think, ans = splitThinking("`code\n\n<think>real</think>Answer")
	if think != "real" || ans != "`code\n\nAnswer" {
		t.Fatalf("paragraph ends span: think=%q ans=%q", think, ans)
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
