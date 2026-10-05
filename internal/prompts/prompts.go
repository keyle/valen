// Package prompts owns the agent guidelines: one embedded file per model
// family (opencode-adapted, valen-tuned), family routing, the <env> header,
// and the plan-mode overlay plus read-only ctx marking.
package prompts

import (
	"context"
	_ "embed"
	"fmt"
	"runtime"
	"strings"
	"time"
)

var (
	//go:embed prompt_meta.txt
	meta string
	//go:embed prompt_default.txt
	def string
	//go:embed prompt_anthropic.txt
	anthropic string
	//go:embed prompt_beast.txt
	beast string
	//go:embed prompt_codex.txt
	codex string
	//go:embed prompt_copilot.txt
	copilot string
	//go:embed prompt_gemini.txt
	gemini string
	//go:embed prompt_gpt.txt
	gpt string
	//go:embed prompt_gpt_astra.txt
	gptAstra string
	//go:embed prompt_kimi.txt
	kimi string
	//go:embed prompt_trinity.txt
	trinity string
	//go:embed prompt_plan.txt
	plan string
	//go:embed prompt_build_switch.txt
	buildSwitch string
)

// ForModel selects the agent guidelines per model family, mirroring
// opencode's system.ts routing: muse→meta, claude→anthropic, old
// reasoning (gpt-4/o1/o3)→beast, gpt-6→astra, gpt/grok→gpt, codex→codex,
// copilot→copilot, gemini→gemini, kimi→kimi, trinity→trinity, else default.
func ForModel(model string) string {
	l := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(l, "muse"):
		return meta
	case strings.Contains(l, "claude"), strings.Contains(l, "anthropic"):
		return anthropic
	case strings.Contains(l, "trinity"):
		return trinity
	case strings.Contains(l, "codex"):
		return codex
	case strings.Contains(l, "copilot"):
		return copilot
	case strings.Contains(l, "gpt-4"), hasOSeries(l):
		return beast
	case strings.Contains(l, "gpt-6"):
		return gptAstra
	case strings.Contains(l, "gpt"), strings.Contains(l, "grok"):
		return gpt
	case strings.Contains(l, "gemini"):
		return gemini
	case strings.Contains(l, "kimi"):
		return kimi
	default:
		return def
	}
}

// hasOSeries matches opencode's o1/o3 routing without catching substrings
// like "mimo" or "protocol": token-boundary match on o1/o3 variants.
func hasOSeries(l string) bool {
	for _, tok := range strings.FieldsFunc(l, func(r rune) bool {
		return r == '-' || r == '_' || r == '/' || r == ' ' || r == ':'
	}) {
		if tok == "o1" || tok == "o3" || strings.HasPrefix(tok, "o1-") || strings.HasPrefix(tok, "o3-") {
			return true
		}
	}
	return strings.HasPrefix(l, "o1") || strings.HasPrefix(l, "o3")
}

// FriendlyName mirrors opencode's {{MODEL_NAME}} for the muse family.
func FriendlyName(model string) string {
	if strings.Contains(strings.ToLower(model), "muse-glimmer") {
		return "Muse Glimmer"
	}
	if strings.Contains(strings.ToLower(model), "muse") {
		return "Muse Spark"
	}
	return model
}

// SystemPrompt builds the full system message: a small env header (like
// opencode's environment block) plus the family prompt file, with the plan
// overlay appended in plan mode.
func SystemPrompt(model string, planMode bool, cwd string) string {
	body := ForModel(model)
	body = strings.ReplaceAll(body, "{{MODEL_NAME}}", FriendlyName(model))
	body = strings.ReplaceAll(body, "{{MODEL_ID}}", model)
	if planMode {
		body += "\n\n" + plan
	}
	body += "\n\n" + outsideRule
	header := fmt.Sprintf("You are powered by the model named %s. The exact model ID is %s.\n<env>\n  Working directory: %s\n  Platform: %s\n  Today's date: %s\n</env>\n\n",
		FriendlyName(model), model, cwd, runtime.GOOS, time.Now().Format("Mon Jan 02 2006"))
	return header + body
}

// outsideRule covers what code cannot: shell commands that reach outside the
// workdir. File tools are gated in files.go; shell is gated in shellgate.go
// (destructive/network labels plus outside-path folders) with the OS sandbox
// as the boundary.
const outsideRule = `# Boundaries — Folders Outside the Workdir
- Sibling git worktrees of this repo (see ` + "`git worktree list`" + `) are in scope: read, edit, and run commands there freely, including creating new ones with ` + "`git worktree add`" + `.
- Shell is sandboxed by the OS: it may write only inside the workdir, sibling worktrees, folders the user approved, the private temp dir and tool caches. Shared /tmp and $HOME are blocked until approved — explicit use asks once per folder, like git push does. A write elsewhere fails with "Operation not permitted" or "Read-only file system".
- Shell has no network by default (localhost works). For downloads, git fetch/push or curl, pass network=true on the shell call; the user is asked once per session. Commands such as git push, git reset --hard, git clean, rm -r and sudo also ask once per session. If denied, stop and tell the user.
- .env files cannot be read or written; ask the user for the values you need. Templates (.env.example) stay readable.
- Safe build commands (go test, go vet, gofmt, ls inside the workdir) never ask: they contain no outside paths. To work in another folder, first call glob or read on it with the file tools: the user is asked once per folder, and shell may then read/write there. If denied, do not try to work around it.
- Never read or modify secrets or dotfiles in the home directory (shell rc files, tokens) without asking. $HOME reads are blocked by the sandbox; an approved folder is re-allowed.`

// BuildSwitch is the notice shown when leaving plan mode.
func BuildSwitch() string {
	return strings.TrimSpace(buildSwitch)
}

type planCtxKey struct{}

// WithPlanMode marks ctx read-only so tool Fns can refuse mutations.
func WithPlanMode(ctx context.Context) context.Context {
	return context.WithValue(ctx, planCtxKey{}, true)
}

// IsPlanMode reports whether ctx carries the plan-mode read-only mark.
func IsPlanMode(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(planCtxKey{}).(bool)
	return v
}

// Refusal is the tool output when plan mode blocks a mutation. Returned
// as a Go error so Registry.Run formats it as tool output (not a turn
// failure) — the model sees it and replans instead of the turn dying.
func Refusal(tool string) string {
	return fmt.Sprintf("plan mode is read-only — %s is blocked. Investigate with read/glob/grep/web/task(explore), present the plan as your reply, and let the user exit plan mode to execute.", tool)
}
