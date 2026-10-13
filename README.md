# valen

A lightweight coding-agent harness in a single Go binary, inspired by
[opencode](https://opencode.ai). It has no server and no runtime dependencies.
The aim is the same agent loop at a small fraction of the resource footprint:
about 50 MB of RAM in use.

valen has a terminal chat UI and a one-shot headless mode. It talks to
[OpenCode Go/Zen](https://opencode.ai) and [OpenRouter](https://openrouter.ai)
models and runs the tool loop locally.

## Features

- Terminal UI (Bubble Tea) and headless `-p` mode
- Tools: `shell`, `read`, `write`, `edit`, `glob`, `grep`, `web_search`,
  `web_fetch`, plus todos and questions
- Stdio MCP servers
- Plan mode (read-only investigation)
- Foreground `task` subagents with isolated history
- Summary-style auto-compaction (on by default, `/compact` for manual)
- Session logs (JSONL) and resume
- Live model and reasoning-effort switching (`/model`, `/effort`)
- Shell sandbox: macOS `sandbox-exec`, Linux `bubblewrap`. It blocks writes
  outside the workdir, hides credential files and `.env` secrets, scrubs
  secret env vars, and asks before network use or destructive commands.

## Install

Requires Go and, on Linux, `bubblewrap`.

```sh
git clone https://github.com/keyle/valen && cd valen
make install         # builds and copies valen to /usr/local/bin
```

## Usage

Set a key, or put it in `~/.config/valen/config.toml`:

```sh
export OPENCODE_API_KEY=...      # OpenCode Go / Zen
export OPENROUTER_API_KEY=...    # OpenRouter
```

```sh
valen                       # TUI in the current directory
valen -p "explain main.go"  # one-shot
valen -plan                 # read-only plan mode
valen -dir ~/proj           # set the working directory
```

Config is seeded on first run at `~/.config/valen/config.toml`. See
[config.toml](config.toml) for options and [SPEC.md](SPEC.md) /
[SPEC-tui.md](SPEC-tui.md) for the full spec.

## Limitations

- **Platforms:** macOS and Linux only. Other OSes (including Windows) fail to
  build rather than run shell commands unsandboxed.
- **Agents:** subagents run in the foreground, one at a time. There are no
  background or concurrent agents. Only read-only tool calls within a single
  model turn run in parallel.
- **Shell safety:** the command prompts (`git push`, `rm -r`, `sudo`, ...) are a
  best-effort speed bump. The OS sandbox is the real boundary.
- **Providers:** OpenCode Go/Zen and OpenRouter only.

This is early software. Review what an agent is allowed to do before pointing
it at anything you care about.

## Development

```sh
gofmt -l . && go vet ./... && go test ./...
```

Tests use offline `httptest` fakes.

## License

[MIT](LICENSE)
