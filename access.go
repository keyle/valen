package main

// access.go: the fixed 4-choice access decision shared by the file gate
// (checkFileAccess) and the shell gate (approveShell). Free text is hidden:
// an access decision is grant/deny x this session/all sessions, nothing else.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

type accessChoice int

const (
	accessAllowSession accessChoice = iota
	accessAllowAlways
	accessDenySession
	accessDenyAlways
)

const (
	lblAllowSession = "Grant access this session"
	lblAllowAlways  = "Grant access FOR ALL SESSIONS"
	lblDenySession  = "Deny access this session"
	lblDenyAlways   = "Deny access FOR ALL SESSIONS"

	lblConfirmYes = "Yes"
	lblConfirmNo  = "No"
)

// allowed reports whether the choice grants access.
func (c accessChoice) allowed() bool { return c == accessAllowSession || c == accessAllowAlways }

// always reports whether the choice persists beyond this session.
func (c accessChoice) always() bool { return c == accessAllowAlways || c == accessDenyAlways }

// session is the session-scoped variant of the choice (what a declined
// confirmation downgrades to).
func (c accessChoice) session() accessChoice {
	switch c {
	case accessAllowAlways:
		return accessAllowSession
	case accessDenyAlways:
		return accessDenySession
	}
	return c
}

// parseAccessChoice matches the chosen label exactly. Anything unrecognized
// (free text, a stale stub, an empty answer) fails closed to a session deny.
func parseAccessChoice(answers []string) accessChoice {
	if len(answers) == 0 {
		return accessDenySession
	}
	switch answers[0] {
	case lblAllowSession:
		return accessAllowSession
	case lblAllowAlways:
		return accessAllowAlways
	case lblDenyAlways:
		return accessDenyAlways
	}
	return accessDenySession
}

// promptAccess asks the 4-choice question about subject (the folder or
// command, shown highlighted). allowRule/denyRule name what an "all sessions"
// grant/deny would write; for those the user is asked once more to confirm,
// and declining downgrades to the session-scoped outcome instead of persisting.
func promptAccess(ctx context.Context, q AskQuestion, subject, allowRule, denyRule string) (accessChoice, error) {
	q.NoFreeText = true
	q.Highlight = []string{subject}
	always := "writes " + accessPathLabel()
	if !accessPersistent() {
		always = "no access.toml here (-config run): acts as this session only"
	}
	q.Options = []AskOption{
		{Label: lblAllowSession, Description: "this session only"},
		{Label: lblAllowAlways, Description: always},
		{Label: lblDenySession, Description: "this session only"},
		{Label: lblDenyAlways, Description: always},
	}
	answers, err := promptUser(ctx, []AskQuestion{q})
	if err != nil {
		return accessDenySession, err
	}
	c := parseAccessChoice(answers)
	if !c.always() {
		return c, nil
	}
	if !accessPersistent() {
		return c.session(), nil
	}
	rule := denyRule
	if c.allowed() {
		rule = allowRule
	}
	ok, err := confirmAccess(ctx, rule)
	if err != nil {
		return c.session(), err
	}
	if !ok {
		return c.session(), nil
	}
	return c, nil
}

// confirmAccess is the "are you sure?" for an all-sessions decision: a single
// yes/no, default yes (first row), that names the exact rule and its file.
// Only an exact "Yes" confirms; anything else declines.
func confirmAccess(ctx context.Context, rule string) (bool, error) {
	answers, err := promptUser(ctx, []AskQuestion{{
		Question:   fmt.Sprintf("Write this rule to %s?", accessPathLabel()),
		Header:     "Are you sure?",
		Highlight:  []string{rule},
		NoFreeText: true,
		Options: []AskOption{
			{Label: lblConfirmYes, Description: "persist it for all future sessions"},
			{Label: lblConfirmNo, Description: "keep it to this session only"},
		},
	}})
	if err != nil {
		return false, err
	}
	return len(answers) > 0 && answers[0] == lblConfirmYes, nil
}

// ---- persistent rules: ~/.config/valen/access.toml ----

const userAccessName = "access.toml"

// Rule kinds: folder lists (prefix-matched, a folder covers its subtree) and
// shell capabilities decided "FOR ALL SESSIONS".
const (
	ruleRead       = "read"
	ruleWrite      = "write"
	ruleDeny       = "deny"
	ruleShellAllow = "shell.allow"
	ruleShellDeny  = "shell.deny"
)

const accessHeader = `# Written by valen's access prompts; hand-editable. Folder grants are
# prefix-matched (a folder covers its subtree); DENY WINS over read/write,
# and secrets (.env*, ~/.ssh, ...) always stay blocked.
#   read  = read-only forever      write = read + write forever
#   deny  = blocked forever        [shell] = capabilities: network, git push, rm -r, sudo
`

type accessRules struct {
	Read  []string    `toml:"read"`
	Write []string    `toml:"write"`
	Deny  []string    `toml:"deny"`
	Shell accessShell `toml:"shell"`
}

type accessShell struct {
	Allow []string `toml:"allow"`
	Deny  []string `toml:"deny"`
}

var (
	accessMu    sync.Mutex
	accessFile  string // "" = session-only (tests, -config <file>)
	accessCache *accessRules
	accessErr   error // parse error of the cached file: saves refuse to clobber it
)

// setAccessFile pins the persistent rules file ("" keeps decisions
// session-only) and drops the cache.
func setAccessFile(path string) {
	accessMu.Lock()
	accessFile, accessCache, accessErr = path, nil, nil
	accessMu.Unlock()
}

func accessPersistent() bool {
	accessMu.Lock()
	defer accessMu.Unlock()
	return accessFile != ""
}

// accessPathLabel names the persistent rules file for prompts.
func accessPathLabel() string {
	accessMu.Lock()
	p := accessFile
	accessMu.Unlock()
	if home, err := os.UserHomeDir(); err == nil && home != "" && withinRoot(home, p) {
		if rel, err := filepath.Rel(home, p); err == nil {
			return filepath.Join("~", rel)
		}
	}
	return p
}

// list returns the slice for a rule kind.
func (r *accessRules) list(kind string) *[]string {
	switch kind {
	case ruleRead:
		return &r.Read
	case ruleWrite:
		return &r.Write
	case ruleDeny:
		return &r.Deny
	case ruleShellAllow:
		return &r.Shell.Allow
	case ruleShellDeny:
		return &r.Shell.Deny
	}
	return nil
}

func (r *accessRules) normalize() {
	for _, k := range []string{ruleRead, ruleWrite, ruleDeny} {
		l := r.list(k)
		out := make([]string, 0, len(*l))
		for _, p := range *l {
			if p = normalizeAccessPath(p); p != "" {
				out = append(out, p)
			}
		}
		*l = out
	}
	for _, k := range []string{ruleShellAllow, ruleShellDeny} {
		l := r.list(k)
		if *l == nil {
			*l = []string{}
		}
	}
}

// normalizeAccessPath expands ~/ and resolves the path to an absolute,
// symlink-resolved form. Relative or empty entries are ignored.
func normalizeAccessPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) {
		return ""
	}
	return real(filepath.Clean(p))
}

// loadAccessLocked reads (once) the rules file. Missing or unreadable
// yields empty rules; a parse error is remembered so saves do not overwrite
// the user's broken file, and its rules are ignored.
func loadAccessLocked() *accessRules {
	if accessCache != nil {
		return accessCache
	}
	r := &accessRules{}
	if accessFile != "" {
		if _, err := toml.DecodeFile(accessFile, r); err != nil && !errors.Is(err, os.ErrNotExist) {
			accessErr = err
			r = &accessRules{}
		}
	}
	r.normalize()
	accessCache = r
	return r
}

// accessDirs returns a copy of one folder-rule list.
func accessDirs(kind string) []string {
	accessMu.Lock()
	defer accessMu.Unlock()
	return append([]string(nil), *loadAccessLocked().list(kind)...)
}

// accessMatch reports whether abs lies inside any folder of the kind.
func accessMatch(kind, abs string) bool {
	for _, dir := range accessDirs(kind) {
		if inApproved(dir, abs) {
			return true
		}
	}
	return false
}

// accessDenied is the persistent deny check; it beats every allow.
func accessDenied(abs string) bool { return accessMatch(ruleDeny, abs) }

// accessShellRule reports a persistent shell-capability decision for label.
func accessShellRule(label string) (allow, deny bool) {
	accessMu.Lock()
	defer accessMu.Unlock()
	r := loadAccessLocked()
	return slices.Contains(r.Shell.Allow, label), slices.Contains(r.Shell.Deny, label)
}

// saveAccessRule adds value to the kind's list (deduplicated, sorted) and
// writes the file atomically. No-op without a rules file (session-only).
func saveAccessRule(kind, value string) error {
	accessMu.Lock()
	defer accessMu.Unlock()
	if accessFile == "" {
		return nil
	}
	r := loadAccessLocked()
	if accessErr != nil {
		return fmt.Errorf("%s is malformed, not overwriting: %w", accessFile, accessErr)
	}
	l := r.list(kind)
	if l == nil {
		return fmt.Errorf("unknown access rule kind %q", kind)
	}
	if kind != ruleShellAllow && kind != ruleShellDeny {
		if value = normalizeAccessPath(value); value == "" {
			return errors.New("access rule needs an absolute path")
		}
	}
	if slices.Contains(*l, value) {
		return nil
	}
	next := *r
	nl := next.list(kind)
	*nl = append(append([]string(nil), *l...), value)
	sort.Strings(*nl)
	if err := writeAccessFile(accessFile, &next); err != nil {
		return err
	}
	accessCache = &next
	return nil
}

func renderAccessRules(r *accessRules) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(accessHeader + "\n")
	cp := *r
	for _, k := range []string{ruleRead, ruleWrite, ruleDeny, ruleShellAllow, ruleShellDeny} {
		if l := cp.list(k); *l == nil {
			*l = []string{}
		}
	}
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(cp); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeAccessFile(path string, r *accessRules) error {
	raw, err := renderAccessRules(r)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".access-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// seedAccessFile writes the commented empty rules file when absent; an
// existing file is never touched.
func seedAccessFile(dir string) error {
	path := filepath.Join(dir, userAccessName)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeAccessFile(path, &accessRules{})
}
