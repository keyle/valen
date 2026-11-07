# Tasks: harness

Status: planned. Nothing implemented. Resume here.

Order: S3.4 → S3.3 → S3.2 → S3.1 → S3.5 → S3.6. Keep one task in progress.
Rules: targeted `go test -run` while iterating; no commits; branch stays local.
Decisions D1–D10 and questions Q1–Q3 are in `design.md`.

## 0. Setup

- [ ] 0.1 `git fetch origin && git worktree add /tmp/valen-harness -b harness-stream origin/master`.
      If a tracked secret blocks it, use the `--no-checkout` recipe from AGENTS.md.
- [ ] 0.2 Baseline in the worktree: `go build ./... && go vet ./...`.
- [ ] 0.3 Get answers to Q1–Q2 from the user (Q3 has a default).

## 1. S3.4 — H5 `extra_body`

- [ ] 1.1 Add `ExtraBody map[string]any` with `toml:"extra_body"` to `Config` (`config.go`, near `:89`).
- [ ] 1.2 `chatWireBody` (`chat.go:137`): do not take the early return at `chat.go:148` when `ExtraBody` is set. Merge the non-reserved keys (D8). Warn on stderr for skipped keys.
- [ ] 1.3 Test `TestExtraBodyRepetitionPenalty` (`sseServer`, `stream_test.go:16`): body contains `"repetition_penalty":1.1`; a reserved key (`model`) is not overridden.
- [ ] 1.4 Docs together: `config.toml` comment, `SPEC.md` config table (near `:95`).
- [ ] 1.5 `go test -run 'TestExtraBody' .` green.

## 2. S3.3 — H3 flags and H4 `--one-shot-task`

- [ ] 2.1 `Registry.Remove(name)` in `tools.go` (map + `names` slice).
- [ ] 2.2 Add bools in `main.go:86-95`: `--stream`, `--no-question`, `--no-task`, `--no-todo`, `--one-shot-task`. `--one-shot-task` sets `printMode`, `noQuestion`, `noTodo` (D7).
- [ ] 2.3 Extract `buildRegistry(cfg, opts)` from `start()` (D10).
- [ ] 2.4 After `LoadBuiltins` (`main.go:205`): remove banned names. Gate the adds at `main.go:209`, `:217`, `:233`.
- [ ] 2.5 Test `TestNoFlagsRemoveTools`: all three flags remove their names.
- [ ] 2.6 Test `TestNoFlagsRemoveConfigAdded`: a config `tools` list that names `AskUserQuestion` / `todowrite` still loses them.
- [ ] 2.7 Test `TestOneShotTaskFlags`: `-p` set, `task` still present, the other two gone, session still written.
- [ ] 2.8 `go test -run 'TestNoFlags|TestOneShot' .` green.

## 3. S3.2 — H2 `--history <file>`

- [ ] 3.1 Add `"history": true` to the `reorderFlags` map (`main.go:104-106`).
- [ ] 3.2 At `chat.go:500-502`: read the file (JSON array, 8 MiB `LimitReader`, D5). Drop `system`-role entries. Bad JSON exits 1 before any model call.
- [ ] 3.3 Add the 8 MiB history cap to `openspec/project.md` caps.
- [ ] 3.4 Test `TestHistoryInOrder`: hand-written file; request body has the history after system and before the new prompt, in order.
- [ ] 3.5 Test `TestHistoryBadJSON`: exits before any request is made.
- [ ] 3.6 `go test -run 'TestHistory' .` green.

## 4. S3.1 — H1 `--stream`

- [ ] 4.1 `--stream` sets `printMode` (`main.go:151`, D1).
- [ ] 4.2 Mutex-guarded NDJSON encoder on stdout. It is the only stdout writer in stream mode.
- [ ] 4.3 Map `Event` (`turn.go:19`) to the spec kinds only (D2): `round_start`, `text_delta` (`text`), `tool_start` (`name`, `id`, `args`), `tool_result` (`name`, `id`, `dur_ms`, no output per D3), `error` (`message`). Drop `thinking_delta`, `retry`, `or_meta`.
- [ ] 4.4 Stdout answer fix: `done.reply` and one-shot stdout use the think-split `answer` (`chat.go:536`), not raw `reply.Content` (`chat.go:566`).
- [ ] 4.5 `done` is last: `reply`, `usage`, `messages` (D4), `error` if any.
- [ ] 4.6 SIGTERM: emit `done` with `error: "cancelled"` if possible, exit non-zero (D9, `main.go:201`).
- [ ] 4.7 Test `TestStreamNDJSONOrder`: each stdout line is valid JSON, `round_start` first, `text_delta` before `done`, `done` last.
- [ ] 4.8 Test `TestStreamUnaryNoTextDelta`: a unary reply emits no `text_delta`, and `done.reply` is correct.
- [ ] 4.9 Test `TestStreamStdoutOnlyNDJSON`: no non-JSON bytes on stdout, including with a thinking-bearing reply.
- [ ] 4.10 Test `TestStreamCancelled`: `done.error == "cancelled"`, non-zero exit.
- [ ] 4.11 `go test -race -run 'TestStream' .` (parallel tools emit events).

## 5. S3.5 — `thinking = "minimal"` probe (manual spike)

- [ ] 5.1 Direct POST to the OpenRouter chat endpoint for `deepseek-v4-flash` with `reasoning_effort: "minimal"`. Needs the user's key and `network=true`. Not a `go test`.
- [ ] 5.2 Record accepted or rejected, with status and error text, in `design.md` Results.
- [ ] 5.3 If rejected: the voice profile uses `low`. valen config already rejects `minimal` (`config.go:220`), so no code change.
- [ ] 5.4 If accepted at the API: adding `minimal` to `models.go:341-342` is a separate small change. Ask first.

## 6. S3.6 — fake-SSE tests

- [ ] 6.1 Helpers: `sseServer` (`stream_test.go:16`), `chatTextStream` (`:34`), `newTestConfig` (`main_test.go:198`).
- [ ] 6.2 Check: NDJSON order (4.7).
- [ ] 6.3 Check: history round-trip. Run 1 `done.messages` → file → run 2 request body has them in order.
- [ ] 6.4 Check: the three flags remove names from the registry (2.5).
- [ ] 6.5 Check: `repetition_penalty` reaches the body (1.3).
- [ ] 6.6 Run `go test -run 'TestStream|TestHistory|TestNoFlags|TestOneShot|TestExtraBody' .` with `network=true`.

## 7. Final gate

- [ ] 7.1 `gofmt -l .` clean.
- [ ] 7.2 `go vet ./...` green.
- [ ] 7.3 `go test ./...` green, `network=true`. Required: this touches `runTurn`, config and the registry.
- [ ] 7.4 `go test -race ./...` for the event writer.
- [ ] 7.5 Leave the branch local. Do not commit or push unless the user asks.
