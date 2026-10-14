package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/keyle/valen/internal/prompts"
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
	deniedAccess     = map[string]bool{} // "r:"+folder / "w:"+folder
)

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

// pathLocks serializes the read-modify-write of write/edit per canonical
// path so parallel subagents cannot lose each other's updates. In-process
// only: shell writes are not covered. One small mutex per touched file.
var pathLocks sync.Map

func lockPath(abs string) (unlock func()) {
	v, _ := pathLocks.LoadOrStore(canonicalAccessPath(abs), new(sync.Mutex))
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
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
			defer lockPath(abs)()
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
			defer lockPath(abs)()
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
