# Change: shell-approval

What `shell` may do is already bounded by the OS sandbox; this change is
about the *human* half: when a command asks, what the ask says, and what the
transcript remembers. It answers the "shell runs unprompted" worry with an
explicit recommendation instead of a silent default.

## Why

`gateShell` today asks before network, five destructive labels, and paths
outside the workdir. Everything inside the workdir runs without a word —
including `git commit`, a `npm run publish` script nobody reviewed, or
destructive work inside a build script the text labels cannot see. Users
cannot opt into asking for everything, and an approval leaves no trace in
the transcript once the question overlay is gone. There is no way to audit
what was granted after the fact.

## Recommendation (the part worth arguing about)

1. **Keep `outside` as the default.** A sandboxed `sh -c` inside the workdir
   is the loop: asking for every `go test`/`gofmt` trains blind approval and
   makes the prompt worthless. The kernel policy, not the prompt, is the
   boundary.
2. **Recommend `always` for shared machines, untrusted repos, and anything
   where "what did it run" matters.** One yes/no per command, no session
   memory unless the user grants "all sessions".
3. **Never offer "allow everything forever" from a single prompt.** The
   existing 4-choice dialog is per label/folder and that stays.
4. **Show every grant.** An approval must leave a committed transcript line
   (`approved shell: git push · session`), so a run can be audited without
   scrollback luck.

## What changes

1. `[shell] prompt = "outside" | "always" | "never"` (default `outside`).
   `always` asks once per command before it starts (still honoring
   `allow`/`deny` rules and the sandbox). `never` disables the prompt
   entirely — sandbox + rules only — and prints a boot warning.
2. Approval events: `Event{Kind: "approval", Label, Decision, Scope,
   Subject}` emitted by `approveShell`/`checkFileAccess`; TUI commits a dim
   line, headless writes `approval: allowed <label> (session)` to stderr.
   Denials are committed too (`denied shell: rm -r`), so the model's next
   clamp-down is explained.
3. Extend the destructive label list: `git push --force` (alongside plain
   `git push`), `chmod -R`, and `curl|sh` / `wget|sh` pipelines. Each label
   keeps its own remember key.
4. The approval question states the bounded scope, e.g. `writes only in
   ~/proj · network off`, so the answer is informed rather than reflexive.
5. Headless stays fail-closed (no prompter → deny); the stderr approval line
   explains why a `-p` run skipped a `git push`. Pre-seed `access.toml` for
   unattended runs; no `-yes` flag.

## Non-goals

- The ordered `allow/ask/deny` rule engine and `.valen/approvals.toml` —
  that is `openspec/changes/permissions`, which subsumes the rule storage
  here.
- Per-command sandbox profiles, seccomp, command allowlists shipped by default.
- Parsing shell ASTs: matching stays best-effort text; the kernel is the
  boundary.

## Success criteria

- `TestShellPromptPolicy` (outside/always/never; never prints warning),
  `TestApprovalEventEmitted` (allow, deny, session scope), `TestShellLabelExtend`
  (`git push --force`, `chmod -R`, `curl|sh`), TUI test that the committed
  line appears once. `-race` green (approval maps).
