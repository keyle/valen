# Change: harness

## Why

The voxtral voice loop and background jobs drive valen as a subprocess. valen
has no machine-readable event stream, no way to pass prior messages in, no
way to drop interactive or delegating tools for one run, and no way to set
extra request-body keys.

Source: `../voxtral/openspec/changes/voice-assistant-explore/specs/harness/spec.md`
(checked at `keyle/valen` commit `e48a948`). Every change is opt-in; default
behaviour does not change.

## What changes

1. **H1 `--stream`**: NDJSON events on stdout, `done` last, nothing else on
   stdout. Implies `-p`.
2. **H2 `--history <file>`**: prior messages (JSON array) placed after the
   system message and before the new prompt.
3. **H3 `--no-question`, `--no-task`, `--no-todo`**: remove those tool names
   from the registry.
4. **H4 `--one-shot-task`**: `-p` + `--no-question` + `--no-todo`. Prints only
   the answer, exit 0 on success, 1 on error, still writes the session JSONL.
5. **H5 `extra_body`**: TOML table merged into the chat-completions body.
   Reserved keys are never overridden.
6. **S3.5**: find out whether DeepSeek Flash accepts `thinking = "minimal"`.
   If not, the voice profile uses `low`.
7. **S3.6**: offline tests against a fake SSE server for the checks above.

## Non-goals

- No server/client split (AGENTS.md rule 6). Everything stays in-process.
- No new runtime dependency.
- No upstreaming PRs (voxtral F2). The branch stays local.
- The voice profile TOML itself lives in voxtral. valen only needs `-config`.
- `tool_result` output is not streamed in v1 (see design D3).

## Success criteria

- All six S3.6 checks pass with targeted `-run` tests (NDJSON order, history
  round-trip, three flags remove names from the registry, `repetition_penalty`
  reaches the body). Run with `network=true`.
- Final gate: `gofmt -l .` clean, `go vet ./...` green, `go test ./...` green.
  `-race` on the NDJSON writer, since parallel tools emit events.
- Branch `harness-stream` is local, not pushed. No commits unless asked.
