package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"valen/internal/prompts"
)

// files.go: local file builtins (read, write, edit, glob, grep) scoped to a
// sandbox root. The root is the process workdir after -dir handling in
// main.go; every path is resolved against it and escapes ask first.

const (
	defaultReadLimit = 2000
	maxReadLimit     = 5000
	// maxReadBytes caps one read below the 16k registry output cap so the
	// offset-continue hint always reaches the model intact (a 50k cap would
	// be eaten by the registry truncate). Ditched the old 200-line default
	// that forced 10 full-history rounds to see a 2000-line file.
	maxReadBytes     = 12 * 1024
	maxGrepMatches   = 100
	maxGrepPerFile   = 20
	maxGrepFileBytes = 1 << 20
	maxReadLineChars = 1000
)

var (
	fileRootMu       sync.Mutex
	fileRoot         string
	approvedMu       sync.Mutex
	approvedRead     = map[string]bool{}
	approvedWritable = map[string]bool{}
)

// setFileRoot pins the sandbox root and drops prior outside approvals.
func setFileRoot(dir string) {
	fileRootMu.Lock()
	fileRoot = dir
	fileRootMu.Unlock()
	approvedMu.Lock()
	approvedRead = map[string]bool{}
	approvedWritable = map[string]bool{}
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

// allowedWithoutPrompt reports read access without asking: inside the root or
// a sibling worktree, or any folder approved for read or write this session.
// Both the lexical path and its symlink-resolved canonical must pass; callers
// check both.
func allowedWithoutPrompt(root, abs string) bool {
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
	return false
}

// allowedWriteWithoutPrompt reports write access without asking: inside the
// root or a sibling worktree, or a folder approved specifically for write or
// edit this session. A read-only approval does not count.
func allowedWriteWithoutPrompt(root, abs string) bool {
	if inRootOrWorktree(root, abs) {
		return true
	}
	for _, dir := range approvedDirs(approvedWritable) {
		if inApproved(dir, abs) {
			return true
		}
	}
	return false
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
	if askPrompter == nil {
		return fmt.Errorf("denied: %s is outside workdir %s (no interactive prompter to approve it)", abs, root)
	}
	access := "read"
	if isWriteOp(op) {
		access = "read and write"
	}
	question := fmt.Sprintf("Allow %s of %s? This grants %s access to all of folder %s. Workdir is %s.", op, abs, access, folder, root)
	if call := toolCallFrom(ctx); call != "" {
		question += "\nRequested by: " + call
	}
	answers, err := promptUser(ctx, []AskQuestion{{
		Question: question,
		Header:   "File access",
		Options: []AskOption{
			{Label: "Allow", Description: "Permit this folder for the rest of this session"},
			{Label: "Deny", Description: "Keep the sandbox as is"},
		},
	}})
	if err != nil {
		return fmt.Errorf("permission prompt failed: %w", err)
	}
	if len(answers) > 0 && strings.Contains(strings.ToLower(answers[0]), "allow") {
		approvedMu.Lock()
		approvedRead[folder] = true
		if isWriteOp(op) {
			approvedWritable[folder] = true
		}
		approvedMu.Unlock()
		return nil
	}
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

func ReadTool() Tool {
	return Tool{
		Name:        "read",
		Description: "Read a file from the workdir. Lines are prefixed with their number (e.g. `12: code`); when editing, use the exact file text without numbers.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file path, relative to the workdir or absolute"},"offset":{"type":"integer","description":"first line to show, 1-indexed (default 1)"},"limit":{"type":"integer","description":"max lines to show (default 2000, max 5000)"}},"required":["path"]}`),
		Fn: func(ctx context.Context, args string) (string, error) {
			var a struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return "", err
			}
			abs, err := resolveFilePath(a.Path)
			if err != nil {
				return "", err
			}
			if err := checkFileAccess(ctx, "read", abs); err != nil {
				return "", err
			}
			if blocksSecret(abs) {
				return "", fmt.Errorf("denied: %s holds secrets and is not readable; ask the user for the values you need", abs)
			}
			raw, err := os.ReadFile(abs)
			if err != nil {
				return "", err
			}
			if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
				return "", fmt.Errorf("%s is a directory, use glob to list it", abs)
			}
			if len(raw) == 0 {
				return fmt.Sprintf("File %s is empty.", relToRoot(abs)), nil
			}
			lines := strings.Split(string(raw), "\n")
			total := len(lines)
			start := a.Offset
			if start < 1 {
				start = 1
			}
			limit := a.Limit
			if limit <= 0 {
				limit = defaultReadLimit
			}
			if limit > maxReadLimit {
				limit = maxReadLimit
			}
			if start > total {
				return "", fmt.Errorf("%s has %d lines, offset %d is past the end", relToRoot(abs), total, start)
			}
			end := min(start+limit-1, total)
			var b strings.Builder
			fmt.Fprintf(&b, "File %s (%d lines, showing %d-%d):\n", relToRoot(abs), total, start, end)
			shown := end
			bytes := 0
			for i := start; i <= end; i++ {
				line := lines[i-1]
				if len(line) > maxReadLineChars {
					line = line[:maxReadLineChars] + "…[long line truncated]"
				}
				row := fmt.Sprintf("%d: %s\n", i, line)
				if bytes+len(row) > maxReadBytes {
					shown = i - 1
					break
				}
				bytes += len(row)
				b.WriteString(row)
			}
			if shown < end {
				fmt.Fprintf(&b, "...[output capped at ~12KB, showing %d-%d: re-read with offset %d to continue]", start, shown, shown+1)
			} else if end < total {
				fmt.Fprintf(&b, "...[%d more lines, read with offset %d]", total-end, end+1)
			}
			return truncate(strings.TrimRight(b.String(), "\n"), maxToolOutput), nil
		},
	}
}

func WriteTool() Tool {
	return Tool{
		Name:        "write",
		Description: "Create or overwrite a file in the workdir, creating parent directories as needed.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file path, relative to the workdir or absolute"},"content":{"type":"string","description":"full file content"}},"required":["path","content"]}`),
		Fn: func(ctx context.Context, args string) (string, error) {
			if prompts.IsPlanMode(ctx) {
				return "", fmt.Errorf("%s", prompts.Refusal("write"))
			}
			var a struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return "", err
			}
			abs, err := resolveFilePath(a.Path)
			if err != nil {
				return "", err
			}
			if err := checkFileAccess(ctx, "write", abs); err != nil {
				return "", err
			}
			if blocksSecret(abs) {
				return "", fmt.Errorf("denied: %s holds secrets and is not writable; ask the user for the values you need", abs)
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return "", err
			}
			mode := os.FileMode(0o644)
			if fi, err := os.Stat(abs); err == nil && !fi.IsDir() {
				mode = fi.Mode().Perm()
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if err := os.WriteFile(abs, []byte(a.Content), mode); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), relToRoot(abs)), nil
		},
	}
}

func EditTool() Tool {
	return Tool{
		Name:        "edit",
		Description: "Replace exact text in a file. oldString must appear verbatim; set replaceAll to rewrite every occurrence.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file path, relative to the workdir or absolute"},"oldString":{"type":"string","description":"exact text to replace"},"newString":{"type":"string","description":"replacement text (may be empty)"},"replaceAll":{"type":"boolean","description":"replace every occurrence (default false)"}},"required":["path","oldString","newString"]}`),
		Fn: func(ctx context.Context, args string) (string, error) {
			if prompts.IsPlanMode(ctx) {
				return "", fmt.Errorf("%s", prompts.Refusal("edit"))
			}
			var a struct {
				Path       string `json:"path"`
				OldString  string `json:"oldString"`
				NewString  string `json:"newString"`
				ReplaceAll bool   `json:"replaceAll"`
			}
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return "", err
			}
			abs, err := resolveFilePath(a.Path)
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(a.Path) == "" || a.OldString == "" {
				return "", fmt.Errorf("path and oldString are required")
			}
			if err := checkFileAccess(ctx, "edit", abs); err != nil {
				return "", err
			}
			if blocksSecret(abs) {
				return "", fmt.Errorf("denied: %s holds secrets and is not editable; ask the user for the values you need", abs)
			}
			raw, err := os.ReadFile(abs)
			if err != nil {
				return "", err
			}
			n := strings.Count(string(raw), a.OldString)
			if n == 0 {
				return "", fmt.Errorf("oldString not found in %s", relToRoot(abs))
			}
			if n > 1 && !a.ReplaceAll {
				return "", fmt.Errorf("found %d matches in %s; provide more context or set replaceAll", n, relToRoot(abs))
			}
			updated := raw
			if a.ReplaceAll {
				updated = []byte(strings.ReplaceAll(string(raw), a.OldString, a.NewString))
			} else {
				updated = []byte(strings.Replace(string(raw), a.OldString, a.NewString, 1))
			}
			mode := os.FileMode(0o644)
			if fi, err := os.Stat(abs); err == nil {
				mode = fi.Mode().Perm()
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if err := os.WriteFile(abs, updated, mode); err != nil {
				return "", err
			}
			if a.ReplaceAll {
				return fmt.Sprintf("replaced %d occurrences in %s", n, relToRoot(abs)), nil
			}
			return fmt.Sprintf("edited %s", relToRoot(abs)), nil
		},
	}
}

func GlobTool() Tool {
	return Tool{
		Name:        "glob",
		Description: "List files under the workdir matching a glob pattern (supports *, ?, [...] and **).",
		Schema:      json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"glob pattern, e.g. **/*.go"},"path":{"type":"string","description":"base directory to search (default .)"}},"required":["pattern"]}`),
		Fn: func(ctx context.Context, args string) (string, error) {
			var a struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
			}
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return "", err
			}
			if strings.TrimSpace(a.Pattern) == "" {
				return "", fmt.Errorf("pattern is required")
			}
			base := a.Path
			if strings.TrimSpace(base) == "" {
				base = "."
			}
			baseAbs, err := resolveFilePath(base)
			if err != nil {
				return "", err
			}
			if err := checkFileAccess(ctx, "list", baseAbs); err != nil {
				return "", err
			}
			fi, err := os.Stat(baseAbs)
			if err != nil {
				return "", err
			}
			if !fi.IsDir() {
				return "", fmt.Errorf("%s is not a directory", relToRoot(baseAbs))
			}
			pattern := filepath.ToSlash(a.Pattern)
			if filepath.IsAbs(a.Pattern) {
				patAbs, err := resolveFilePath(a.Pattern)
				if err != nil {
					return "", err
				}
				if err := checkFileAccess(ctx, "list", patAbs); err != nil {
					return "", err
				}
				rel, err := filepath.Rel(getFileRoot(), patAbs)
				if err != nil {
					return "", err
				}
				pattern = filepath.ToSlash(rel)
			}
			var out []string
			// Top-level subtrees walk in parallel (syscall-bound); matches merge
			// and sort for deterministic output.
			entries, err := os.ReadDir(baseAbs)
			if err != nil {
				return "", err
			}
			var mu sync.Mutex
			match := func(rel string) {
				if matchGlob(pattern, rel) {
					mu.Lock()
					out = append(out, rel)
					mu.Unlock()
				}
			}
			var dirs []string
			for _, e := range entries {
				if e.IsDir() && skippedDir(e.Name()) {
					continue
				}
				p := filepath.Join(baseAbs, e.Name())
				if !e.IsDir() {
					if rel, err := filepath.Rel(getFileRoot(), p); err == nil {
						match(filepath.ToSlash(rel))
					}
					continue
				}
				dirs = append(dirs, p)
			}
			const globWorkers = 8
			jobs := make(chan string)
			var wg sync.WaitGroup
			for w := 0; w < globWorkers; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for dir := range jobs {
						_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
							if err != nil {
								return nil
							}
							if d.IsDir() {
								if skippedDir(d.Name()) {
									return filepath.SkipDir
								}
								return nil
							}
							rel, err := filepath.Rel(getFileRoot(), p)
							if err != nil {
								return nil
							}
							match(filepath.ToSlash(rel))
							return nil
						})
					}
				}()
			}
			for _, d := range dirs {
				jobs <- d
			}
			close(jobs)
			wg.Wait()
			sort.Strings(out)
			if len(out) == 0 {
				return "no matches", nil
			}
			return truncate(strings.Join(out, "\n"), maxToolOutput), nil
		},
	}
}

func GrepTool() Tool {
	return Tool{
		Name:        "grep",
		Description: "Search file contents in the workdir with a Go regexp. Returns file:line matches.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"Go regexp to search for"},"path":{"type":"string","description":"file or directory to search (default .)"},"include":{"type":"string","description":"optional glob filter, e.g. *.go"}},"required":["pattern"]}`),
		Fn: func(ctx context.Context, args string) (string, error) {
			var a struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
				Include string `json:"include"`
			}
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return "", err
			}
			if strings.TrimSpace(a.Pattern) == "" {
				return "", fmt.Errorf("pattern is required")
			}
			re, err := regexp.Compile(a.Pattern)
			if err != nil {
				return "", fmt.Errorf("bad pattern: %w", err)
			}
			target := a.Path
			if strings.TrimSpace(target) == "" {
				target = "."
			}
			targetAbs, err := resolveFilePath(target)
			if err != nil {
				return "", err
			}
			if err := checkFileAccess(ctx, "search", targetAbs); err != nil {
				return "", err
			}
			roots := []string{targetAbs}
			if fi, err := os.Stat(targetAbs); err != nil {
				return "", err
			} else if fi.IsDir() {
				roots = []string{targetAbs}
			}
			// Phase 1: serial walk collects candidate files (cheap metadata
			// prefilters only). Phase 2 reads and matches them on a bounded
			// worker pool, since file I/O dominates.
			var files []string
			for _, root := range roots {
				err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
					if err != nil {
						return nil
					}
					if d.IsDir() && skippedDir(d.Name()) {
						return filepath.SkipDir
					}
					if d.IsDir() {
						return nil
					}
					// Symlink files resolve before reading: link-env ->
					// .env.probe-test must skip like the target, and links
					// escaping the root/approved/worktrees must not leak.
					if blocksSecret(p) {
						return nil
					}
					if canon := canonicalAccessPath(p); !allowedWithoutPrompt(getFileRoot(), canon) {
						return nil
					}
					rel, err := filepath.Rel(getFileRoot(), p)
					if err != nil {
						return nil
					}
					slash := filepath.ToSlash(rel)
					if a.Include != "" && !matchGlob(filepath.ToSlash(a.Include), slash) && !matchGlob(filepath.ToSlash(a.Include), path.Base(slash)) {
						return nil
					}
					if fi, err := d.Info(); err == nil && fi.Size() > maxGrepFileBytes {
						return nil
					}
					files = append(files, p)
					return nil
				})
				if err != nil {
					return "", err
				}
			}
			matches, capped := grepFilesParallel(ctx, re, files)
			if len(matches) > maxGrepMatches {
				matches = matches[:maxGrepMatches]
				capped = true
			}
			if len(matches) == 0 {
				return "no matches", nil
			}
			out := strings.Join(matches, "\n")
			if capped {
				out += fmt.Sprintf("\n...[showing first %d matches]", maxGrepMatches)
			}
			return truncate(out, maxToolOutput), nil
		},
	}
}

// grepFilesParallel reads and matches candidate files on a bounded worker
// pool, returning matches sorted by file then line for deterministic output.
// The second return value reports whether more matches existed past
// maxGrepMatches: workers stop appending at the cap but keep scanning cheaply
// enough to notice the overflow.
// grepMatch is one match for deterministic file-then-line sorting.
type grepMatch struct {
	file string
	line int
	text string
}

func grepFilesParallel(ctx context.Context, re *regexp.Regexp, files []string) ([]string, bool) {
	const workers = 8
	root := getFileRoot()
	var mu sync.Mutex
	var matches []grepMatch
	var capped bool
	jobs := make(chan string)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				if ctx.Err() != nil {
					continue
				}
				mu.Lock()
				done := len(matches) >= maxGrepMatches && capped
				mu.Unlock()
				if done {
					continue
				}
				raw, err := os.ReadFile(p)
				if err != nil {
					continue
				}
				if bytes.IndexByte(raw, 0) != -1 {
					continue
				}
				rel, err := filepath.Rel(root, p)
				if err != nil {
					continue
				}
				slash := filepath.ToSlash(rel)
				var local []grepMatch
				for i, line := range strings.Split(string(raw), "\n") {
					if !re.MatchString(line) {
						continue
					}
					text := strings.TrimSpace(line)
					if len(text) > 200 {
						text = text[:200] + "…"
					}
					local = append(local, grepMatch{file: slash, line: i + 1, text: text})
				}
				if len(local) == 0 {
					continue
				}
				if len(local) > maxGrepPerFile {
					local = local[:maxGrepPerFile]
					mu.Lock()
					capped = true
					mu.Unlock()
				}
				mu.Lock()
				room := maxGrepMatches - len(matches)
				if room < 0 {
					room = 0
				}
				if len(local) > room {
					local = local[:room]
					capped = true
				}
				matches = append(matches, local...)
				mu.Unlock()
			}
		}()
	}
	for _, f := range files {
		jobs <- f
	}
	close(jobs)
	wg.Wait()
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].file != matches[j].file {
			return matches[i].file < matches[j].file
		}
		return matches[i].line < matches[j].line
	})
	out := make([]string, 0, len(matches))
	for _, gm := range matches {
		out = append(out, fmt.Sprintf("%s:%d: %s", gm.file, gm.line, gm.text))
	}
	return out, capped
}

// skippedDir reports dependency/build output directories that grep and glob
// never descend into — searching node_modules or target/ wastes the tool
// round-trip and floods history with third-party code. Only the exact
// .git-style hard skip existed before; ripgrep users get this via
// .gitignore, so the builtin needs the same baseline.
func skippedDir(name string) bool {
	switch name {
	case ".git",
		"node_modules",
		"target",
		"dist",
		"build",
		"bin",
		"vendor",
		".venv", "venv",
		"__pycache__",
		".next",
		"out",
		"coverage":
		return true
	}
	return false
}

// matchGlob matches rel (slash-separated) against a pattern with *, ?, [...]
// per path segment plus ** for any depth. Both pattern and rel use /.
func matchGlob(pattern, rel string) bool {
	pSegs := strings.Split(pattern, "/")
	rSegs := strings.Split(rel, "/")
	return matchSegs(pSegs, rSegs)
}

func matchSegs(p, r []string) bool {
	if len(p) == 0 {
		return len(r) == 0
	}
	if p[0] == "**" {
		for i := 0; i <= len(r); i++ {
			if matchSegs(p[1:], r[i:]) {
				return true
			}
		}
		return false
	}
	if len(r) == 0 {
		return false
	}
	ok, err := path.Match(p[0], r[0])
	if err != nil || !ok {
		return false
	}
	return matchSegs(p[1:], r[1:])
}
