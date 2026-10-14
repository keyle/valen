package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/keyle/valen/internal/dvlog"
)

// gate serializes check -> prompt -> record for approvals, so parallel
// subagents hitting the same unapproved folder or shell label prompt once and
// the rest reuse the answer (allow or deny). Ctx-aware: a cancelled call
// stops waiting behind someone else's prompt.
var gate = make(chan struct{}, 1)

func lockGate(ctx context.Context) (unlock func(), err error) {
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// setFileRoot pins the sandbox root and drops prior outside approvals.
func setFileRoot(dir string) {
	fileRootMu.Lock()
	fileRoot = dir
	fileRootMu.Unlock()
	approvedMu.Lock()
	approvedRead = map[string]bool{}
	approvedWritable = map[string]bool{}
	deniedAccess = map[string]bool{}
	approvedMu.Unlock()
	resetShellApprovals()
}

// getFileRoot returns the sandbox root, falling back to the process cwd.
func getFileRoot() string {
	fileRootMu.Lock()
	defer fileRootMu.Unlock()
	if fileRoot != "" {
		return fileRoot
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}

// resolveFilePath cleans p against the sandbox root and returns an absolute
// path. Containment is checked separately so callers can prompt on escapes.
func resolveFilePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(getFileRoot(), p)
	}
	return filepath.Clean(p), nil
}

// withinRoot reports whether abs stays inside root (both absolute, clean).
func withinRoot(root, abs string) bool {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// worktreeCacheTTL bounds how stale a worktree list may be. Worktrees get
// created mid-session, so refresh after this window rather than spawning
// `git worktree list` on every sandbox build and outside-root access check.
const worktreeCacheTTL = 2 * time.Second

type worktreeEntry struct {
	dirs []string
	at   time.Time
}

var (
	worktreeMu    sync.Mutex
	worktreeCache = map[string]worktreeEntry{}
)

// siblingWorktrees lists the checkout paths of the root's git repo (main plus
// linked worktrees), memoized briefly: the list is stable between worktree
// commands but this runs on every shell command and file access check.
func siblingWorktrees(root string) []string {
	worktreeMu.Lock()
	if e, ok := worktreeCache[root]; ok && time.Since(e.at) < worktreeCacheTTL {
		worktreeMu.Unlock()
		return e.dirs
	}
	worktreeMu.Unlock()
	dirs := listSiblingWorktrees(root)
	worktreeMu.Lock()
	worktreeCache[root] = worktreeEntry{dirs: dirs, at: time.Now()}
	worktreeMu.Unlock()
	return dirs
}

// resetWorktreeCache drops cached lists after a command that can change them
// (git worktree add/remove/prune), so a new worktree is usable immediately
// instead of waiting out the TTL.
func resetWorktreeCache() {
	worktreeMu.Lock()
	worktreeCache = map[string]worktreeEntry{}
	worktreeMu.Unlock()
}

func listSiblingWorktrees(root string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", root, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return nil
	}
	var dirs []string
	for _, l := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(l, "worktree "); ok {
			dirs = append(dirs, filepath.Clean(p))
		}
	}
	return dirs
}

// real resolves symlinks when possible (macOS /var vs /private/var).
func real(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// inSiblingWorktree reports whether abs lies in a worktree of the same repo.
func inSiblingWorktree(root, abs string) bool {
	abs = real(abs)
	for _, wt := range siblingWorktrees(root) {
		if withinRoot(real(wt), abs) {
			return true
		}
	}
	return false
}

// accessFolder is the unit of approval: abs itself when it is a directory,
// else its parent, so one answer covers a whole folder subtree.
func accessFolder(abs string) string {
	if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
		return abs
	}
	return filepath.Dir(abs)
}

// canonicalAccessPath resolves symlinks for access checks: the exact path
// when it exists (follows link files), else the resolved parent joined with
// the base name (covers writes through a symlinked dir and new files).
// Falls back to abs when nothing exists yet.
func canonicalAccessPath(abs string) string {
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	dir := filepath.Dir(abs)
	if rd, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Join(rd, filepath.Base(abs))
	}
	return abs
}

// inRootOrWorktree reports whether abs is inside the root or a sibling git
// worktree of the same repo. Comparisons try the stored root and its resolved
// form (macOS /var -> /private/var): temp roots like t.TempDir() are lexical
// /var/... while canonicalAccessPath returns /private/..., which must match.
func inRootOrWorktree(root, abs string) bool {
	for _, r := range []string{root, real(root)} {
		if r != "" && withinRoot(r, abs) {
			return true
		}
	}
	return inSiblingWorktree(root, abs)
}

// inApproved reports whether abs lies inside dir, trying the stored path and
// its symlink-resolved form.
func inApproved(dir, abs string) bool {
	if withinRoot(dir, abs) {
		return true
	}
	if rd := real(dir); rd != "" && rd != dir && withinRoot(rd, abs) {
		return true
	}
	return false
}

// approvedDirs snapshots an approval set.
func approvedDirs(m map[string]bool) []string {
	approvedMu.Lock()
	defer approvedMu.Unlock()
	dirs := make([]string, 0, len(m))
	for dir := range m {
		dirs = append(dirs, dir)
	}
	return dirs
}

// isWriteOp reports whether a checkFileAccess op changes the filesystem.
func isWriteOp(op string) bool { return op == "write" || op == "edit" }

// allowedWithoutPrompt reports read access without asking: any folder approved
// for read or write (this session or in access.toml) and inside the root or a
// sibling worktree, unless persistently denied. A persistent deny is checked
// first and beats every allow, workdir included. Both the lexical path and its
// symlink-resolved canonical must pass; callers check both.
func allowedWithoutPrompt(root, abs string) bool {
	// Persistent deny beats every allow, including the workdir shortcut:
	// otherwise the kernel hides a denied folder from shell but read/write
	// still reach it through the file tools.
	if accessDenied(abs) {
		return false
	}
	if inRootOrWorktree(root, abs) {
		return true
	}
	for _, dir := range approvedDirs(approvedRead) {
		if inApproved(dir, abs) {
			return true
		}
	}
	for _, dir := range approvedDirs(approvedWritable) {
		if inApproved(dir, abs) {
			return true
		}
	}
	return accessMatch(ruleRead, abs) || accessMatch(ruleWrite, abs)
}

// allowedWriteWithoutPrompt reports write access without asking: a folder
// approved specifically for write or edit this session (a read-only approval
// does not count), or inside the root or a sibling worktree — unless
// persistently denied (checked first, workdir included).
func allowedWriteWithoutPrompt(root, abs string) bool {
	if accessDenied(abs) {
		return false
	}
	if inRootOrWorktree(root, abs) {
		return true
	}
	for _, dir := range approvedDirs(approvedWritable) {
		if inApproved(dir, abs) {
			return true
		}
	}
	return accessMatch(ruleWrite, abs)
}

// blocksSecret reports whether reading/writing abs would touch a secret
// .env file, following symlinks: link-env -> .env.probe-test must deny even
// though the link name looks innocent.
func blocksSecret(abs string) bool {
	if isSecretFile(abs) {
		return true
	}
	if c := canonicalAccessPath(abs); c != abs && isSecretFile(c) {
		return true
	}
	return false
}

// toolCallKey carries a short description of the tool call being executed so
// permission prompts can say what the model is trying to do, not just which
// path it touched.
type toolCallKey struct{}

// withToolCall annotates ctx with a human-readable summary of a tool call.
func withToolCall(ctx context.Context, summary string) context.Context {
	if strings.TrimSpace(summary) == "" {
		return ctx
	}
	return context.WithValue(ctx, toolCallKey{}, summary)
}

func toolCallFrom(ctx context.Context) string {
	s, _ := ctx.Value(toolCallKey{}).(string)
	return s
}

// oneLine collapses whitespace runs (newlines included) and truncates, so a
// multi-line script or file body fits in a prompt.
func oneLine(s string, n int) string {
	return truncateRunes(strings.Join(strings.Fields(s), " "), n)
}

// summarizeToolCall renders what a tool call is about to do for a prompt:
// the shell command, a write's content preview, an edit's old -> new text,
// or the raw arguments for anything else.
func summarizeToolCall(name, args string) string {
	var a map[string]any
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return name + ": " + oneLine(args, 300)
	}
	str := func(k string) string { s, _ := a[k].(string); return s }
	switch name {
	case "shell":
		if c := str("command"); c != "" {
			return "shell: " + oneLine(c, 300)
		}
	case "write":
		return fmt.Sprintf("write %d bytes: %s", len(str("content")), oneLine(str("content"), 150))
	case "edit":
		return fmt.Sprintf("edit: replace %q with %q", oneLine(str("oldString"), 100), oneLine(str("newString"), 100))
	}
	return name + ": " + oneLine(args, 300)
}

// checkFileAccess allows paths inside the root, inside a sibling worktree of
// the same repo, or inside a previously approved folder; anything else asks
// the user (per folder) via the active AskUserQuestion prompter. Without an
// interactive frontend there is nobody to ask, so the access is denied.
func checkFileAccess(ctx context.Context, op, abs string) error {
	root := getFileRoot()
	canon := canonicalAccessPath(abs)
	allowed := allowedWithoutPrompt
	if isWriteOp(op) {
		allowed = allowedWriteWithoutPrompt
	}
	if allowed(root, abs) && allowed(root, canon) {
		return nil
	}
	// Prompt for the resolved target when the link escapes: approving the
	// link name alone must not grant the outside folder.
	outside := abs
	if !allowed(root, canon) {
		outside = canon
	}
	folder := accessFolder(outside)
	if accessDenied(folder) || accessDenied(abs) || accessDenied(canon) {
		return fmt.Errorf("denied: access to %s is blocked by %s; do not retry or work around it", abs, accessPathLabel())
	}
	if askPrompter == nil {
		return fmt.Errorf("denied: %s is outside workdir %s (no interactive prompter to approve it)", abs, root)
	}
	unlock, err := lockGate(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	// Another call may have answered while we waited for the gate.
	if allowed(root, abs) && allowed(root, canon) {
		return nil
	}
	denyKey := "r:" + folder
	if isWriteOp(op) {
		denyKey = "w:" + folder
	}
	approvedMu.Lock()
	denied := deniedAccess[denyKey] || (isWriteOp(op) && deniedAccess["r:"+folder])
	approvedMu.Unlock()
	if denied {
		return fmt.Errorf("denied: access to %s was not approved (earlier answer this session); do not retry or work around it", abs)
	}
	access, kind := "read", "read"
	if isWriteOp(op) {
		access, kind = "read and write", "write"
	}
	question := fmt.Sprintf("Allow %s of %s? This grants %s access to all of folder %s. Workdir is %s.", op, abs, access, folder, root)
	if call := toolCallFrom(ctx); call != "" {
		question += "\nRequested by: " + call
	}
	choice, err := promptAccess(ctx, AskQuestion{Question: question, Header: "File access"}, folder, fmt.Sprintf("%s = %s", kind, folder), "deny = "+folder)
	if err != nil {
		return fmt.Errorf("permission prompt failed: %w", err)
	}
	if choice.always() { // persist first, then book the session maps below
		rule := ruleDeny
		if choice.allowed() {
			rule = kind
		}
		if err := saveAccessRule(rule, folder); err != nil {
			dvlog.Logf("access: persist %s %s: %v", rule, folder, err)
		}
	}
	if choice.allowed() {
		approvedMu.Lock()
		approvedRead[folder] = true
		if isWriteOp(op) {
			approvedWritable[folder] = true
		}
		approvedMu.Unlock()
		return nil
	}
	approvedMu.Lock()
	deniedAccess[denyKey] = true
	approvedMu.Unlock()
	return fmt.Errorf("denied: access to %s was not approved", abs)
}

// relToRoot renders abs for model output, relative when inside the root.
func relToRoot(abs string) string {
	rel, err := filepath.Rel(getFileRoot(), abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return abs
	}
	return filepath.ToSlash(rel)
}
