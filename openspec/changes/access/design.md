# Design: access

## 1. Current state (verified)

- **FS policy** `sandboxPolicy` (`sandbox.go:35`) has one `Network bool`
  (`sandbox.go:42`); no bind/ipc distinction. `writableRoots()` (`sandbox.go:215`)
  grants `root, TempDir, /tmp, GOCACHE, UserCacheDir, ~/{go,.cache,.npm,.cargo}`
  + session approvals + sibling worktrees. `readableRoots()` (`sandbox.go:239`)
  only session read approvals. `hiddenPaths()` (`sandbox.go:296`) denies `$HOME`
  and the credential set incl. `.ssh .aws .gnupg .kube .docker .azure .netrc
  .npmrc .pypirc ~/.config/gh ~/.config/gcloud ~/.config` + histories;
  `.env*` denied separately (`sandbox_darwin.go:110`, `findSecretFiles`).
- **Session-only approvals** `approvedRead/approvedWritable/deniedAccess`
  (`files.go:43-45`), reset by `setFileRoot` (`files.go:64`). No persistence.
- **File gate** `checkFileAccess` (`files.go:350`) builds a 2-option Allow/Deny
  question (`files.go:397-404`) and books it in session maps.
- **Shell gate** `approveShell` (`shellgate.go:106`) does the same for the
  destructive/`network` labels (`shellgate.go:24,138-145`).
- **Ask layer** `AskQuestion{Question,Header,MultiSelect,Options}`
  (`ask.go:24`). TUI overlay `askState` (`tui.go:184`) always renders an own
  row at index `len(Options)` (`tui.go:676,825`); headless always prints it
  (`ask.go:165`) and `parsePicks` reserves `own=len+1` (`ask.go:231`). No way
  to hide it, no subject styling.
- **User config** `userConfigDir()` = `$XDG_CONFIG_HOME/valen` or
  `~/.config/valen` (`userconfig.go:35`); `ensureUserConfig` seeds
  `config.toml` + `config-ori.toml` (`userconfig.go:98`). Atomic tmp+rename
  writers already exist (slots/state/models). `loadConfig` strict-decodes,
  unknown key = error (`main.go:182-194`); `[input]` must stay the last table
  (`config.toml:81`).
- **Loopback** Darwin: `(deny network*)` + `(allow network* (remote ip
  "localhost:*"))` (`sandbox_darwin.go:100-106`) → loopback *egress* works with
  network off, bind does not. Linux: `--unshare-net` (`sandbox_linux.go:61`)
  → loopback down, nothing binds.

## 2. Ask question additions (`ask.go`, `tui.go`)

```go
type AskQuestion struct {
    Question    string      `json:"question"`
    Header      string      `json:"header,omitempty"`
    MultiSelect bool        `json:"multiSelect,omitempty"`
    Options     []AskOption `json:"options"`
    // Highlight lists the subjects to emphasize: TUI renders each in bold
    // yellow, headless prints it plain. Used so an access prompt makes the
    // affected folder/command unmistakable. Not in the tool schema.
    Highlight []string `json:"highlight,omitempty"`
    // NoFreeText hides the "type your own answer" row: access questions are a
    // fixed choice, so free text would only invite a bad answer. Model-driven
    // questions keep it (zero value false).
    NoFreeText bool `json:"-"`
}
```

- TUI `renderAsk` (`tui.go:777`): if `NoFreeText`, skip the own row
  (`tui.go:825-833`); the subject line renders `Highlight` entries with a new
  `accessSubjectStyle = lipgloss.NewStyle().Bold(true).Foreground(mkYellow)`.
- TUI `handleAskKey` (`tui.go:669`): `rows`/`overflow` account for the missing
  own row, so `enter` at the last real option answers rather than opening the
  "own" phase.
- Headless `headlessOne` (`ask.go:149`): only print/accept the own row when
  free text is allowed; `parsePicks` takes `own` (0 = none). Existing model
  questions are unchanged (default keeps the row).

## 3. Access decision helper (new `access.go`)

```go
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
)
```

`promptAccess(ctx, q AskQuestion, subject string) (accessChoice, error)`:
1. Force `q.NoFreeText = true`; fill the 4 options (descriptions name the
   scope: "this session only" vs "writes ~/.config/valen/access.toml").
2. `promptUser` → parse the chosen label exactly (fail closed to
   `accessDenySession` on anything unrecognized).
3. For `...Always`, call `confirmAccess(ctx, subject, action)`: a single
   yes/no question default **yes** ("Write this rule to
   ~/.config/valen/access.toml?"). `No` downgrades to the session variant.

The confirm is a second `promptUser` call (fresh overlay / stdin prompt),
reusing `askMu` serialization — no new overlay machinery.

## 4. Persistent rules — `~/.config/valen/access.toml`

```toml
# Written by valen's access prompts; hand-editable. Folder grants are
# prefix-matched (a folder covers its subtree); DENY WINS over read/write,
# and secrets always stay blocked.

read  = []   # read-only forever
write = []   # read + write forever
deny  = []   # blocked forever (read and write)

[shell]      # capabilities decided "FOR ALL SESSIONS"
allow = []   # network, git push, rm -r, sudo
deny  = []
```

- Path: `filepath.Join(userConfigDir(), "access.toml")`, resolved through
  `real()` and `~/` expanded on load; stored absolute. Seeded empty (commented)
  alongside `config.toml` in `ensureUserConfig` (`userconfig.go:98`); never
  overwritten once present.
- `loadAccessRules()` is cached in-process and invalidated by
  `saveAccessRule()`. Writes reuse the existing atomic tmp+rename pattern.
- Dedupe on save; keep a stable, sorted order so diffs are minimal.

### Merge / precedence

1. **Secrets first, always:** `blocksSecret` / `.env*` deny and the
   `hiddenPaths` credential set are evaluated last and win over every allow
   (Darwin re-denies after allows; Linux masks after binds — unchanged).
2. **Deny wins:** a match in `deny` (persistent) or `deniedAccess` (session)
   blocks, regardless of any allow.
3. **Allow:** a match in persistent `read`/`write` or session
   `approvedRead/approvedWritable` permits. `write` grants both; `read` grants
   reads only.
4. Not-matched falls through to the existing prompt path.

Wire-in points:
- `allowedWithoutPrompt` (`files.go:255`): consult persistent read+write (after
  deny).
- `allowedWriteWithoutPrompt` (`files.go:275`): persistent write only.
- `checkFileAccess` (`files.go:350`): check persistent deny before booking the
  session deny; on Always, persist then book the session map so the rest of the
  session is instant.
- `writableRoots()`/`readableRoots()` (`sandbox.go:215,239`): add existing
  persistent write/read dirs so mid-session "always" immediately widens the
  kernel policy (rebuilt every `sandboxCommand`, no restart).
- `hiddenPaths()` (`sandbox.go:296`): add persistent deny dirs.
- `shellSafePrefixes()` (`shellgate.go:201`): add persistent read/write dirs so
  shell stops prompting for them.
- `approveShell` (`shellgate.go:106`): consult persistent `[shell]` allow/deny
  before prompting; on Always persist.
- `resetShellApprovals` (`shellgate.go:41`) keeps session maps; persistent
  rules are separate and are not reset by `setFileRoot` (that is the point).

## 5. Seeded sandbox roots + loopback bind + ipc socket

- Add `fvm`, `.fvm`, `.pub-cache`, `.rustup`, `.gradle` to the `$HOME` loop in
  `writableRoots()` (`sandbox.go:223`) and `shellSafePrefixes()`
  (`shellgate.go:211`); they stay subject to `dropBroadRoots`.
- New `sandboxPolicy.AllowBind bool`. Darwin appends
  `(allow network-bind (local ip "localhost:*"))` after the existing
  `(deny network*)` when `!Network && AllowBind` (`sandbox_darwin.go:100-106`).
- `sandboxPolicy.IPCPaths []string`: re-allow reads+writes to each socket
  literal after the `$HOME` deny (Darwin `literal` rule; Linux `--bind`), plus
  ancestor metadata so the client can `connect(2)`.
- New optional `[sandbox]` table in `config.toml` (place **before** `[input]`,
  which must stay last; `config.toml:81`):

```toml
[sandbox]
# allow = ["/opt/homebrew"]        # extra rw roots (built-in toolchains still granted)
# deny  = []                        # extra hidden paths (wins over allow + built-ins)
# allow_bind = ["127.0.0.1", "::1"] # loopback binds with network egress off (default on)
# ipc_allow  = ["~/.colima/default/docker.sock", "/var/run/docker.sock"] # default on
```

Struct `SandboxConfig{Allow []string; Deny []string; AllowBind []string;
IPCAllow []string}` in `main.go` (`Config`, `main.go:75`). Empty/omitted =
built-in defaults (bind + docker sockets on). This is the harness policy file;
`access.toml` is the machine-written record of user decisions.

## 6. Linux loopback bind (limitation to confirm)

`--unshare-net` (`sandbox_linux.go:61`) creates a netns whose `lo` is down, so
a bind-only grant needs `lo` brought up inside the sandbox. bwrap cannot do
this portably (needs `CAP_NET_ADMIN` in the userns and an `ip` binary).
**Plan:** implement Darwin first-class; on Linux keep `allow_bind` honored only
when it can be done without a new dep, otherwise documented as "use
`network: true`", and make `TestSandboxAllowBindLoopback` `t.Skip` when the OS
cannot. Flag for decision (see §8).

## 7. Docs & tests

- Update `SPEC.md` (sandbox capabilities, access.toml, prompt options) and the
  `config.toml` comments (`config.toml:90-103`) + `sandboxHint`
  (`sandbox.go:126`) together.
- Tests: `ask_test.go` (4 options, no own row, confirm default yes, both TUI
  and headless), `access_test.go` (round-trip, deny-wins, malformed answer fails
  closed), `sandbox_test.go` (seeded roots, bind on/off, ipc literal,
  deny-wins vs secrets), `shellgate` (always allow/deny). Keep all offline:
  `httptest` fakes, no real network; bind test uses a loopback `net.Listen`.

## 8. Decisions & open questions

- **Assumed:** `[shell]` lives in `access.toml` (the 4-choice prompt is shared
  with shell), not a separate file.
- **Assumed:** confirm-`No` falls back to the session outcome (not cancel).
- **Assumed:** built-in bind + docker-socket defaults are **on**; config can
  narrow them.
- **Assumed:** access.toml uses `read`/`write`/`deny` lists (prefix match)
  rather than a per-entry table; simpler to hand-edit.
- **Open:** Linux bind-only support (§6) — acceptable to document as
  Darwin-only for now?
- **Open:** should an `access.toml` deny also block the `read` *tool* prompt
  path and the kernel, or only skip the prompt? Plan: both (kernel hides it).
- **Relationship:** the unfinished `permissions` change adds a general
  `allow/ask/deny` evaluator; keep these separate to stay terse. If both land,
  `access.toml` rules become the evaluator's `always` layer.

## 9. Verification

- Targeted: `go test -run 'TestAccess|TestSandbox|TestGateShell|TestAsk' .`
  then `-race` on the approval/rule maps.
- Manual: fresh `~/.config/valen/access.toml` seeded; approve a folder "FOR ALL
  SESSIONS", confirm, restart, re-run `ls` there without a prompt; deny one
  forever and confirm it still fails closed; `go test ./...` with
  `network: false` binds under `allow_bind`.
- Final gate: `gofmt -l .`, `go vet ./...`, `go test ./...`
  (`network=true` only for the existing loopback fixtures).
