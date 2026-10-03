package main

// Prompt-before-run for shell. The OS sandbox (sandbox.go) bounds WHERE a
// command can write and whether it can reach the network; this file covers
// WHAT it does inside those bounds. Network use and a short list of
// destructive commands each ask the user once per session. The command-text
// match is best-effort (a script can hide `git push`); the sandbox is the
// boundary, this is the speed bump for honest mistakes.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// destructive maps a label (also the remembered key) to a command pattern.
var destructive = []struct {
	label string
	re    *regexp.Regexp
}{
	{"git push", regexp.MustCompile(`\bgit\b[^|;&\n]*\bpush\b`)},
	{"git reset --hard", regexp.MustCompile(`\bgit\b[^|;&\n]*\breset\b[^|;&\n]*--hard`)},
	{"git clean", regexp.MustCompile(`\bgit\b[^|;&\n]*\bclean\b`)},
	{"rm -r", regexp.MustCompile(`\brm\b[^|;&\n]*\s-[a-zA-Z]*[rR]|\brm\b[^|;&\n]*--recursive`)},
	{"sudo", regexp.MustCompile(`(^|[|;&(]\s*)sudo\b`)},
}

var (
	shellMu       sync.Mutex
	shellApproved = map[string]bool{} // "network" or a destructive label
)

func resetShellApprovals() {
	shellMu.Lock()
	shellApproved = map[string]bool{}
	shellMu.Unlock()
}

// destructiveLabels returns the labels of every pattern command matches.
func destructiveLabels(command string) []string {
	var out []string
	for _, d := range destructive {
		if d.re.MatchString(command) {
			out = append(out, d.label)
		}
	}
	return out
}

// gateShell asks (once per session per label) before network use or a
// destructive command. Without an interactive frontend it denies.
func gateShell(ctx context.Context, command string, network bool) error {
	need := destructiveLabels(command)
	if network {
		need = append(need, "network")
	}
	for _, label := range need {
		if err := approveShell(ctx, label, command); err != nil {
			return err
		}
	}
	return nil
}

func approveShell(ctx context.Context, label, command string) error {
	shellMu.Lock()
	ok := shellApproved[label]
	shellMu.Unlock()
	if ok {
		return nil
	}
	if askPrompter == nil {
		return fmt.Errorf("denied: %q needs approval and there is no interactive prompter", label)
	}
	what := "run `" + label + "`"
	if label == "network" {
		what = "give shell commands network access"
	}
	answers, err := promptUser(ctx, []AskQuestion{{
		Question: fmt.Sprintf("Allow valen to %s for the rest of this session? Command: %s", what, truncateRunes(command, 200)),
		Header:   "Shell",
		Options: []AskOption{
			{Label: "Allow", Description: "Permit this for the rest of this session"},
			{Label: "Deny", Description: "Do not run the command"},
		},
	}})
	if err != nil {
		return err
	}
	if len(answers) > 0 && strings.Contains(strings.ToLower(answers[0]), "allow") {
		shellMu.Lock()
		shellApproved[label] = true
		shellMu.Unlock()
		return nil
	}
	return fmt.Errorf("denied: the user did not allow %q; do not retry or work around it", label)
}
