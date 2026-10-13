# valen

A lightweight coding-agent harness in a single Go binary, inspired by
[opencode](https://opencode.ai). It has no server and no runtime dependencies.
The aim is the same agent loop at a small fraction of the resource footprint:
about 50 MB of RAM in use, and less than 10% cpu per agent running.

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
- **Agents:** subagents run in the foreground, nest one level deep, and up to 5
  `task` calls in one model turn run in parallel. There are no background
  agents. `write`/`edit` lock per file; `shell` writes are not locked, so
  parallel tasks should work on separate files.
- **Shell safety:** the command prompts (`git push`, `rm -r`, `sudo`, ...) are a
  best-effort speed bump. The OS sandbox is the real boundary.
- **Providers:** OpenCode Go/Zen and OpenRouter only.

*This is early software* -- 'works for me'. I've been hacking at it for a while. 

Review what an agent is allowed to do before pointing
it at anything you care about.

## Development

```sh
	make
	make install
```

Tests use offline `httptest` fakes.

## License

[MIT](LICENSE)
