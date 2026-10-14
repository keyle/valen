# Change: access

## Why

Every blocked tool currently costs a round-trip. Two causes dominate:

1. **FS roots are a single opaque `$HOME` deny.** Toolchain/cache dirs that
   builds need (`~/fvm`, `~/.fvm`, `~/.pub-cache`, `~/go`, `~/.cargo`,
   `~/.rustup`, `~/.gradle`, `~/.npm`, `~/.cache`) are all under `$HOME`, so a
   block re-prompts folder by folder instead of being pre-allowed. `git`,
   `go`, `dart`, `cargo`, JVM and npm all lstat their own caches here.
2. **Loopback *bind* is not separable from network egress.** `sandbox_darwin.go`
   and `sandbox_linux.go` gate loopback bind behind the whole `network` toggle,
   so `go test ./...` (which binds an `httptest` port) forces a full-egress
   grant it never needed. Same shape for the docker socket (testcontainers,
   `sqlc.sh`): it lives under `$HOME` / `/var/run` and is denied wholesale.

The prompts themselves are also lossy: they offer only allow/deny for this
session, hide the affected folder inside a long sentence, and always append a
free-text escape hatch that makes no sense for a fixed access decision. There
is no way to say "always" — so the same folder is approved again next run.

## What changes

1. **Fixed access decision + no free text.** Access prompts (`checkFileAccess`,
   `approveShell`) become a 4-choice question, reuse of `AskUserQuestion` with
   a new `NoFreeText` flag (the "type your own answer" row is hidden for that
   question only; model questions keep it):
   - `Grant access this session`
   - `Grant access FOR ALL SESSIONS`
   - `Deny access this session`
   - `Deny access FOR ALL SESSIONS`
2. **"Are you sure?" for all-sessions.** Picking either `... FOR ALL SESSIONS`
   opens a second yes/no prompt, default **yes**, that names the exact rule and
   the file it will be written to. `No` downgrades to the session-scoped
   outcome instead of persisting.
3. **Persistent decisions in `~/.config/valen/access.toml`.** Folder grants
   (read / write) and denials, plus shell capabilities (`network`, `git push`,
   `rm -r`, `sudo`), survive restarts. Prefix-matched; **deny wins over allow**;
   secrets (`.env*`, `~/.ssh`, ...) still win over everything.
4. **Clear subject, bold yellow.** The prompt shows the affected folder or
   command on its own line, emphasized (TUI: bold yellow via `mkYellow`;
   headless: plain), with the model name / workdir / requested-by context.
5. **Pre-seeded sandbox roots.** Built-in rw roots gain `~/fvm`, `~/.fvm`,
   `~/.pub-cache`, `~/.rustup`, `~/.gradle` (alongside the existing `go`,
   `.cache`, `.npm`, `.cargo`, temp, cache). Secrets denylist stays and wins.
6. **Loopback bind + docker socket as their own capabilities.** A
   `net.allow_bind` allow (`127.0.0.1`, `::1`) works with network egress off;
   an `ipc.allow` list re-exposes the docker sockets (colima + Desktop).
   Both default on, configurable in `[sandbox]`.

## Non-goals

- CEL/JS policy expressions, per-agent overlays, org policy, server sync.
- A general sandbox `allow`/`ask`/`deny` evaluator over every tool (that is the
  separate `permissions` change; this one only pre-seeds roots + persists the
  folder/shell decisions the existing gates already make).
- Windows (unsupported, `sandbox_other.go` still refuses).
- Making Linux loopback bind first-class if bwrap cannot bring `lo` up (see
  design §6); sharing a remote docker daemon or publishing sockets.

## Success criteria

- `go test -run 'TestAccess|TestSandbox|TestGateShell|TestAsk' .` green; `-race`
  green on the shared approval map + access rule cache.
- Prompt fixtures assert exactly 4 options, no own row, subject highlighted,
  confirm default yes; a persistent allow/deny round-trips through
  `access.toml` and survives a fresh `setFileRoot`.
- `TestSandboxAllowBindLoopback` binds with `network=false` (Darwin; skipped
  where the OS cannot) and `TestSandboxAllowBindOff` still blocks it.
- Deny-wins: a persisted deny overrides a session allow, and a broad allow
  never re-exposes `.env*`, `~/.ssh`, `~/.aws`.
