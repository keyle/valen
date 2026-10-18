# Design: shell-approval

## 1. Where the policy lives

`gateShell` keeps its current shape; a new `shellPromptMode()` reads
`[shell] prompt` once per turn from the captured `Config` (same rule as
every other knob: a running turn does not see a config change).

```
outside  (default) ask for: network, destructive labels, outside paths
always              ask for: every command with no rule/session grant
never               ask for: nothing (rules + sandbox only)
```

`never` is opt-in and logs one boot line to stderr and the TUI notice area:
`shell prompts off — sandbox only`. It is still fail-closed where the
sandbox cannot enforce (Linux without `bwrap` → `shell` errors as today).

`always` does not mean "ask even after I said yes": an `access.toml` allow
or a session grant short-circuits it, otherwise the 4-choice dialog would
re-ask a command the user just approved. In-workdir asks use the label
`shell`, so "Grant access this session" is the explicit escape hatch that
turns `always` back into `outside` for the rest of the session.

## 2. Question text

```
Allow valen to run shell commands?
Command: rm -rf build
Scope: writes only in ~/proj · network off · 60s
```

Scope comes from the live sandbox config (`writableRoots`, `network`);
truncated to 120 runes. The command line keeps its own 200-rune cap.

## 3. Approval events

`Event{Kind: "approval", Text: label, Output: decision, ID: scope}` is too
generic; add explicit fields instead:

```go
// On Event:
Label    string // "git push", "network", "shell", "read:/etc"
Decision string // "allowed" | "denied" | "auto"
Scope    string // "session" | "always" | "rule"
Subject  string // the command or path, already one-line/truncated
```

Emit points: `approveShell` (after the decision), `checkFileAccess`
(approval path only — not the fast allow path), so the transcript shows
grants but not every `read`. `auto` covers a rule/session short-circuit when
`prompt` is `always` (the user sees why no question appeared).

Headless: `runTurn`'s event sink prints `approval: allowed git push
(session) — git push origin main` to stderr; stdout stays the answer.

TUI: `tui_update.go` appends the line to `pendingApprovals`; `turnDoneMsg`
commits them into `Turn.Approvals []string` (JSONL, `omitempty`) and the
transcript renders them dim, above the tool rows of that turn. Resume shows
them again.

## 4. Labels

Add to `destructive` (each keeps its own `shellApproved` key):

| label | match |
| --- | --- |
| `git push --force` | `git … push … --force` / `-f` |
| `chmod -R` | `chmod` with `-R`/`--recursive` |
| `curl\|sh` | `(curl\|wget) … \| (sh\|bash\|zsh)` |

Plain `git push` keeps its existing broader match, so `--force` matches
both labels and asks for both once (same session map, keyed per label).

The matching stays regex text matching; the sandbox is the boundary. A
script that hides `git push` still cannot write outside the workdir without
a kernel denial.

## 5. Interaction with `permissions`

`permissions` will evaluate ordered rules before `approveShell`. This change
only changes *defaults and visibility*, so it can land first: with no rules
file, behavior is exactly the mode table above. When `permissions` lands,
`prompt = "always"` becomes sugar for a `"shell" = "ask"` rule and the mode
key is kept for compatibility.

## 6. Tests

`TestShellPromptPolicy` (mode selection; `never` emits warning),
`TestApprovalEventEmitted` (allow/deny/auto fields), `TestShellLabelExtend`
(force push, `chmod -R`, `curl|sh`), `TestApprovalLineCommitted` (TUI:
one dim line per decision, survives `turnDoneMsg`), `TestHeadlessApprovalStderr`.
All with stubbed `askPrompter`; no real shell beyond `true`/`false` builtins.
