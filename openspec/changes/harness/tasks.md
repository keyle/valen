# Tasks: harness

Status: implemented in the working tree on branch `voice-harness` (in place,
not a `/tmp` worktree). Pending: S3.5 manual probe (needs a key) and the final
gate.

Rules: targeted `go test -run` while iterating; no commits; branch stays local.
Decisions D1–D10 and questions Q1–Q3 are in `design.md`.

## 0. Setup

- [x] 0.1 Worked in the existing working tree (branch `voice-harness`); no
      separate worktree was created.
- [x] 0.2 Baseline: `go build ./... && go vet ./...`.
- [x] 0.3 Q1/Q2 defaults taken: session pollution accepted, callers set
      `system` (Q3 omitted `tool_result` output in v1).

## 1. S3.4 — H5 `extra_body`

- [x] 1.1 `ExtraBody map[string]any` with `toml:"extra_body"` in `Config`.
- [x] 1.2 `chatWireBody`: the early return is bypassed when `ExtraBody` is
      set; reserved keys are validated at config load, not merged.
- [x] 1.3 Tests: `TestExtraBodyReachesChatRequest`,
      `TestExtraBodyRejectsReservedAndNonChatTransport`,
      `TestExtraBodyLoadsOnChatTransport`.
- [x] 1.4 `config.toml` comment and `SPEC.md` config table updated.
- [x] 1.5 `go test -run 'TestExtraBody' .` green.

## 2. S3.3 — H3 flags and H4 `--one-shot-task`

- [x] 2.1 Deviation: no `Registry.Remove`; `toolSet.excludes` filters the
      config list and gates the always-present adds instead.
- [x] 2.2 Bools added; `--one-shot-task` sets `-p` + `--no-question` +
      `--no-todo` (D7).
- [x] 2.3 `buildRegistry` extracted from `start()` (D10).
- [x] 2.4 Gates run after `LoadBuiltins` and cover `AskUserQuestion`, todo and
      `task`.
- [x] 2.5 `TestBuildRegistryToolSet`.
- [x] 2.6 Config-added interactive/todo names still lose to the flags
      (covered by 2.5).
- [x] 2.7 `TestOneShotTaskProfileAndHistory` (+ denial test).
- [x] 2.8 `go test -run 'TestBuildRegistryToolSet|TestOneShot' .` green.

## 3. S3.2 — H2 `--history <file>`

- [x] 3.1 `"history": true` in the `reorderFlags` map.
- [x] 3.2 `loadHistoryFile`: JSON array, 8 MiB size cap, user/assistant only.
- [x] 3.3 8 MiB `--history` cap added to `openspec/project.md`.
- [x] 3.4 `TestOneShotTaskProfileAndHistory` checks order.
- [x] 3.5 `TestLoadHistoryFileKeepsOnlyUserAndAssistant`; bad roles/JSON fail
      before any request.
- [x] 3.6 `go test -run 'TestLoadHistory|TestOneShot' .` green.

## 4. S3.1 — H1 `--stream`

- [x] 4.1 `--stream` is headless-only (TUI mode rejects it).
- [x] 4.2 NDJSON encoder on stdout; one writer, no extra mutex needed (events
      are emitted from the caller's loop).
- [x] 4.3 Event kinds mapped; `thinking_delta`, `retry`, `or_meta` are not in
      the spec stream.
- [x] 4.4 stdout answer uses the think-split `answer`.
- [x] 4.5 `done` last with reply, usage, messages and error.
- [ ] 4.6 SIGTERM `done` with `error: "cancelled"`: not implemented; the
      signal context cancels the turn and the normal error path reports it.
- [x] 4.7 `TestStreamWritesNDJSONWithDoneLast`.
- [x] 4.8 Unary text covered by the stream tests (`done.reply`).
- [x] 4.9 `TestStreamWritesNDJSONWithDoneLast` parses every stdout line.
- [ ] 4.10 `TestStreamCancelled`: not implemented (no cancellation test).
- [x] 4.11 `go test -race -run 'TestStream' .` green.

## 5. S3.5 — `thinking = "minimal"` probe (manual spike)

- [ ] 5.1 Direct POST for `deepseek-v4-flash` with `reasoning_effort`; needs
      the user's key and `network=true`.
- [ ] 5.2 Record accepted/rejected in `design.md` Results.
- [x] 5.3 If rejected: the voice profile uses `low`; `minimal` is not in
      `deepseek-v4-flash`'s accepted set.
- [ ] 5.4 If accepted: add `minimal` to the model table (separate change).

## 6. S3.6 — fake-SSE tests

- [x] 6.1 `sseServer`, `chatTextStream`, `newTestConfig` reused.
- [x] 6.2 NDJSON order.
- [x] 6.3 History round-trip: `--history` CLI test sends it in order.
- [x] 6.4 Flags remove names from the registry.
- [x] 6.5 `repetition_penalty` reaches the body.
- [x] 6.6 Targeted suite green.

## 7. Final gate

- [x] 7.1 `gofmt -l .` clean.
- [x] 7.2 `go vet ./...` green.
- [x] 7.3 `go test ./...` green, `network=true`.
- [x] 7.4 `go test -race ./...` green (child-process request recording moved
      to channels so the detector sees the cross-process edge).
- [x] 7.5 Branch stays local. Do not commit or push unless the user asks.
