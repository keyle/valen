# Tasks: access

## Phase 1 — access prompt UX (no persistence yet)

- [x] 1. `AskQuestion` gains `Highlight []string` + `NoFreeText bool`
  (`ask.go`). TUI `renderAsk`/`handleAskKey` (`tui.go:669,777`) skip the own row
  and render `Highlight` bold yellow (`accessSubjectStyle`, `mkYellow`);
  headless `headlessOne`/`parsePicks` (`ask.go:149,231`) make `own` optional.
  `TestAskNoFreeTextHidesOwnRow`, `TestAskHighlightSubject` (TUI + headless).
- [x] 2. `access.go`: choice constants/labels + exact-label parse (fail closed
  to deny), `promptAccess` (forces `NoFreeText`, 4 options) and `confirmAccess`
  (single yes/no, default yes). `TestAccessChoiceParsing`,
  `TestAccessConfirmDefaultsYes`.
- [x] 3. Route `checkFileAccess` (`files.go:350-420`) and `approveShell`
  (`shellgate.go:106-159`) through `promptAccess`; underline the subject: file
  folder vs write mode, or the shell command/capability. Keep session maps and
  messages. Update `TestGateShellApprovalNamesModel`,
  `TestSandbox*` prompter stubs to the new labels.

## Phase 2 — persistent access.toml

- [x] 4. `~/.config/valen/access.toml` store: `loadAccessRules` (cached) +
  `saveAccessRule` (atomic tmp+rename, dedupe, sorted); expand `~/`, `real()`
  paths. Seed a commented empty file in `ensureUserConfig`
  (`userconfig.go:98`), never overwrite. `TestAccessTomlRoundTrip`,
  `TestAccessTomlSeedOnce`.
- [x] 5. Apply always-decisions end to end: persist then book the session map;
  consult persistent rules in `allowedWithoutPrompt` /
  `allowedWriteWithoutPrompt` (`files.go:255,275`), `writableRoots` /
  `readableRoots` / `hiddenPaths` (`sandbox.go:215,239,296`),
  `shellSafePrefixes` (`shellgate.go:201`) and `approveShell`. Deny wins over
  session and persistent allow; secrets still win over everything.
  `TestAccessDenyWinsOverAllow`, `TestAccessAlwaysWidensSandbox`,
  `TestGateShellAllSessionsPersist`.
- [x] 6. Confirm-`No` downgrades to the session outcome; `--config <file>` runs
  keep access.toml session-only (no user config dir). `TestAccessConfirmNoDowngrades`.

## Phase 3 — seeded roots + capabilities

- [x] 7. Add `fvm`, `.fvm`, `.pub-cache`, `.rustup`, `.gradle` to the `$HOME`
  loop in `writableRoots` (`sandbox.go:223`) and `shellSafePrefixes`
  (`shellgate.go:211`); still subject to `dropBroadRoots`.
  `TestSandboxSeededToolchainRoots`.
- [x] 8. `sandboxPolicy.AllowBind`: Darwin adds
  `(allow network-bind (local ip "localhost:*"))` after `(deny network*)`
  (`sandbox_darwin.go:100`); Linux honored only where bwrap can (else document
  `network: true`). `TestSandboxAllowBindLoopback` (skip where unsupported),
  `TestSandboxAllowBindOff` still blocks.
- [x] 9. `sandboxPolicy.IPCPaths` for the docker sockets (colima + Desktop):
  re-allow rw literal + ancestor metadata after the `$HOME` deny (Darwin) /
  bind (Linux). `TestSandboxDockerSocketReallowed`.
- [x] 10. Optional `[sandbox]` config table (`allow/deny/allow_bind/ipc_allow`)
  placed before `[input]` (which must stay the last table, `config.toml:81`);
  `SandboxConfig` on `Config` (`main.go:75`); empty = built-in defaults;
  `TestSandboxConfigOverrides`.

## Phase 4 — docs + gate

- [x] 11. Docs together: `SPEC.md` (prompt options, access.toml schema, bind/ipc
  capabilities), `config.toml` comments (`:90-103`), `sandboxHint`
  (`sandbox.go:126`).
- [x] 12. Gate: `gofmt -l .` clean, `go vet ./...` green, targeted
  `go test -run 'TestAccess|TestSandbox|TestGateShell|TestAsk' .` then full
  `go test ./...` (`network=true` for the existing loopback fixtures); `-race`
  on the approval map + rule cache.
- [ ] 13. Verify outside a nested sandbox: on Linux (with `bwrap`) run
  `go test -run 'TestAccess|TestSandbox|TestGateShell' .` and check denied-folder
  masking, the docker socket bind (after masks) and `.env` masking; on macOS run
  the skipped `TestAccessDenyHidesFolderFromShell`,
  `TestSandboxAllowBindLoopback`, `TestSandboxAllowBindOff`. Then update the
  "Platform differences" note in `SPEC.md` ("Shell sandbox").

## Phase 5 — review fixes (post-implementation)

- [x] 14. Deny wins inside the workdir: `allowedWithoutPrompt` /
  `allowedWriteWithoutPrompt` (`files.go`) check the persistent deny before the
  workdir/worktree shortcut, so the file gate matches the kernel.
  `TestAccessDenyInsideWorkdirNotBypassed`.
- [x] 15. `allow_bind` addresses are honored: `sandboxPolicy.BindAddrs`
  replaces the bool, one `(allow network-bind (local ip "<addr>:*"))` per
  loopback address (`sandbox_darwin.go`); `bindAddrs()` defaults to `localhost`
  and re-filters loopback. `TestSandboxAllowBindProfile`,
  `TestSandboxBindAddrsConfig`.
- [x] 16. Headless confirm defaults yes: `AskQuestion.DefaultFirst` makes an
  empty Enter pick row 0. `TestAccessConfirmHeadlessDefaultsYes`.
- [x] 17. Session-only prompts (`-config <file>`) offer just the two session
  choices instead of all-sessions labels that act session-only.
  `TestAccessSessionOnlyWithoutFile`.
- [x] 18. Cancelling the confirm (esc / headless "n") downgrades like `No`
  instead of erroring: `errAskDismissed` sentinel. `TestAccessConfirmDismissDowngrades`.
- [x] 19. `$HOME` is never a writable root: `dropBroadRoots` runs after the
  session/persistent grants too (`sandbox.go`), so a broad grant can't re-bind
  home. `TestSandboxBroadGrantNeverBindsHome`.
- [x] 20. `accessDirs(ruleRead)` also carries `[sandbox] allow` (it is rw).

