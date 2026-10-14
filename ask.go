package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
)

// AskOption is one suggested answer to a model-driven question.
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// AskQuestion is one model-driven question. Options carries the model's
// suggestions; the harness always adds its own "type your own" escape hatch.
type AskQuestion struct {
	Question    string      `json:"question"`
	Header      string      `json:"header,omitempty"`
	MultiSelect bool        `json:"multiSelect,omitempty"`
	Options     []AskOption `json:"options"`
	// Highlight lists subjects to emphasize, each on its own line under the
	// question: TUI renders bold yellow, headless plain. Access prompts use it
	// so the affected folder/command is unmistakable. Not in the tool schema.
	Highlight []string `json:"highlight,omitempty"`
	// NoFreeText hides the "type your own answer" row: access questions are a
	// fixed choice. Model-driven questions keep it (zero value false).
	NoFreeText bool `json:"-"`
}

// AskFunc collects answers for model-driven questions. Exactly one is
// installed per run: the TUI blocks on an overlay, headless chat reads stdin,
// eval harnesses stub scripted answers.
type AskFunc func(ctx context.Context, qs []AskQuestion) ([]string, error)

var askPrompter AskFunc

// askMu serializes interactive prompts so parallel tool calls cannot
// interleave questions (single TUI overlay, single stdin).
var askMu sync.Mutex

// promptUser runs the active AskFunc under the prompt mutex. Parallel tool
// batches stay parallel for non-interactive work and serialize only here.
func promptUser(ctx context.Context, qs []AskQuestion) ([]string, error) {
	askMu.Lock()
	defer askMu.Unlock()
	if askPrompter == nil {
		return nil, errors.New("AskUserQuestion: no interactive prompter available")
	}
	return askPrompter(ctx, qs)
}

type askArgs struct {
	Questions []AskQuestion `json:"questions"`
}

// formatAskAnswers renders the tool output the model expects back, on every
// transport (the transport layer wraps the same string per its protocol).
func formatAskAnswers(qs []AskQuestion, answers []string) string {
	pairs := make([]string, 0, len(qs))
	for i, q := range qs {
		a := ""
		if i < len(answers) {
			a = answers[i]
		}
		pairs = append(pairs, fmt.Sprintf("%q=%q", q.Question, a))
	}
	return fmt.Sprintf("User has answered your questions: %s. You can now continue with the user's answers in mind.", strings.Join(pairs, ", "))
}

// AskUserQuestionTool lets the model ask the user questions with suggested
// options, a free-text escape hatch, and a confirmation pass before the
// answers go back on the wire.
func AskUserQuestionTool() Tool {
	return Tool{
		Name:        "AskUserQuestion",
		Description: "Ask the user one or more questions with suggested options. Answers are collected interactively and returned to you.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","items":{"type":"object","properties":{"question":{"type":"string","description":"the question text"},"header":{"type":"string","description":"short section label"},"multiSelect":{"type":"boolean","description":"allow several selections"},"options":{"type":"array","items":{"type":"object","properties":{"label":{"type":"string","description":"choice label"},"description":{"type":"string","description":"what the choice means"}},"required":["label"]}}},"required":["question","options"]}}},"required":["questions"]}`),
		Fn: func(ctx context.Context, args string) (string, error) {
			var a askArgs
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return "", fmt.Errorf("bad arguments: %w", err)
			}
			if len(a.Questions) == 0 {
				return "", errors.New("no questions provided")
			}
			answers, err := promptUser(ctx, a.Questions)
			if err != nil {
				return "", err
			}
			if len(answers) != len(a.Questions) {
				return "", fmt.Errorf("AskUserQuestion: got %d answers for %d questions", len(answers), len(a.Questions))
			}
			return formatAskAnswers(a.Questions, answers), nil
		},
	}
}

// headlessAskInput chooses where interactive answers come from. A piped
// prompt consumes stdin, so when stdin is not a terminal it falls back to
// the controlling terminal when one is available.
func headlessAskInput() (io.Reader, func(), error) {
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		return os.Stdin, func() {}, nil
	}
	f, err := os.OpenFile("/dev/tty", os.O_RDONLY, 0)
	if err != nil {
		return nil, nil, errors.New("AskUserQuestion: no interactive terminal for answers")
	}
	return f, func() { f.Close() }, nil
}

// headlessAsk prompts on stderr and reads answers from r (stdin). Options are
// numbered; the last row is "type your own answer". Multi-select takes
// comma-separated numbers. A confirmation pass can restart or cancel.
func headlessAsk(ctx context.Context, qs []AskQuestion, r io.Reader, w io.Writer) ([]string, error) {
	br := bufio.NewReader(r)
	for {
		answers := make([]string, 0, len(qs))
		for i := range qs {
			a, err := headlessOne(ctx, br, w, &qs[i])
			if err != nil {
				return nil, err
			}
			answers = append(answers, a)
		}
		fmt.Fprintln(w, "\nReview your answers:")
		for i, q := range qs {
			fmt.Fprintf(w, "  %s → %s\n", q.Question, answers[i])
		}
		fmt.Fprint(w, "Submit? [Y/n/e]: ")
		line, err := readLine(br)
		if err != nil {
			return nil, errors.New("no stdin for answers")
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "y", "yes":
			return answers, nil
		case "e", "edit":
			continue
		case "n", "no":
			return nil, errors.New("user dismissed the questions")
		default:
			fmt.Fprintln(w, "please answer y, n or e")
		}
	}
}

func headlessOne(ctx context.Context, br *bufio.Reader, w io.Writer, q *AskQuestion) (string, error) {
	label := q.Header
	if label == "" {
		label = q.Question
	}
	fmt.Fprintf(w, "\n? %s\n", q.Question)
	for _, h := range q.Highlight {
		fmt.Fprintf(w, "  %s\n", h)
	}
	if label != q.Question && q.Header != "" {
		fmt.Fprintf(w, "(%s)\n", label)
	}
	for j, o := range q.Options {
		if o.Description != "" {
			fmt.Fprintf(w, "  %d. %s — %s\n", j+1, o.Label, o.Description)
		} else {
			fmt.Fprintf(w, "  %d. %s\n", j+1, o.Label)
		}
	}
	own := 0 // 0: no free-text row
	if !q.NoFreeText {
		own = len(q.Options) + 1
		fmt.Fprintf(w, "  %d. Type your own answer\n", own)
	}
	if q.MultiSelect {
		fmt.Fprint(w, "choose comma-separated numbers: ")
	} else {
		fmt.Fprint(w, "> ")
	}
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		line, err := readLine(br)
		if err != nil {
			return "", errors.New("no stdin for answers")
		}
		picks, err := parsePicks(line, len(q.Options), own)
		if err != nil {
			fmt.Fprintln(w, "  "+err.Error())
			continue
		}
		if !q.MultiSelect && len(picks) != 1 {
			fmt.Fprintln(w, "  pick one number")
			continue
		}
		if q.MultiSelect {
			ownSelected := false
			for _, p := range picks {
				if p == own {
					ownSelected = true
				}
			}
			if ownSelected && len(picks) > 1 {
				fmt.Fprintln(w, "  type-your-own cannot be combined with options")
				continue
			}
			if ownSelected {
				return askOwn(br, w, label)
			}
			var chosen []string
			for _, p := range picks {
				chosen = append(chosen, q.Options[p-1].Label)
			}
			if len(chosen) == 0 {
				fmt.Fprintln(w, "  pick at least one")
				continue
			}
			return strings.Join(chosen, ", "), nil
		}
		if picks[0] == own {
			return askOwn(br, w, label)
		}
		return q.Options[picks[0]-1].Label, nil
	}
}

func askOwn(br *bufio.Reader, w io.Writer, label string) (string, error) {
	fmt.Fprintf(w, "Your answer for %q: ", label)
	line, err := readLine(br)
	if err != nil || strings.TrimSpace(line) == "" {
		return "", errors.New("no answer given")
	}
	return strings.TrimSpace(line), nil
}

// parsePicks turns a comma-separated list of option numbers into picks. An
// "own" pick is only valid alone in multi-select mode; own 0 means there is
// no free-text row.
func parsePicks(line string, options, own int) ([]int, error) {
	hi := options
	if own > hi {
		hi = own
	}
	var out []int
	for _, part := range strings.Split(line, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 || n > hi {
			return nil, fmt.Errorf("enter number(s) 1–%d", hi)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("enter number(s) 1–%d", hi)
	}
	return out, nil
}

func readLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line != "" {
			return line, nil
		}
		return "", err
	}
	return line, nil
}
