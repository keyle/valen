# valen

A lightweight coding-agent harness in a single Go binary, inspired by
[OpenCode](https://opencode.ai). It has no server and no runtime dependencies.
The aim is the same agent loop at a small fraction of the resource footprint:
about 50 MB of RAM in use, and less than 10% cpu per agent running.

valen has a terminal chat UI and a one-shot headless mode. It talks to
[OpenCode Go/Zen](https://opencode.ai) and [OpenRouter](https://openrouter.ai)
models and runs the tool loop locally.

## Features

- Terminal UI (Bubble Tea) and headless `-p` mode
- `@` fuzzy picker for workdir files and folders, including subfolders
- Attachments: the `@` picker, drag-and-drop paths and ctrl+v screenshots
  ride the turn (text inlined into the prompt, images sent on the wire)
- Tools: `shell`, `read`, `write`, `edit`, `glob`, `grep`, `web_search`,
  `web_fetch`, plus todos and questions
- Stdio MCP servers
- Plan mode (read-only investigation)
- Foreground `task` subagents with isolated history, up to 5 in parallel
- Summary-style auto-compaction (on by default, `/compact` for manual)
- Session logs (JSONL) and resume
- Live model, reasoning-effort and OpenRouter routing switching (`/model`,
  `/effort`, `/route` or ctrl+y cycles none/floor/nitro/exacto)
- Favourite slots: `alt+1`…`alt+0` jump to a saved model + thinking level +
  routing, `alt+shift+1`…`0` stores the current one (`~/.config/valen/slots.yaml`;
  the terminal must send Option/Alt as Meta). See `/help`.
- Shell sandbox: macOS `sandbox-exec`, Linux `bubblewrap`. It blocks writes
  outside the workdir, hides credential files and `.env` secrets, scrubs
  secret env vars, and asks before network use or destructive commands.

Picking OpenRouter's routing affinity (`:floor`/`:nitro`/`:exacto`) as a live,
per-model toggle is rare among harnesses — most expose it only through startup
config, if at all. Try a fast OpenRouter model with `high` effort and `nitro`
(ctrl+y) and it flies!

## Install

Requires Go and, on Linux, `bubblewrap`.

```sh
git clone https://github.com/keyle/valen && cd valen
make install         # builds and copies valen to /usr/local/bin
```

## Usage

Set a key, or put it in `~/.config/valen/valen.toml`:

```sh
export OPENCODE_API_KEY=...      # OpenCode Go / Zen
export OPENROUTER_API_KEY=...    # OpenRouter
```

```sh
valen                       # TUI in the current directory
valen -p "explain main.go"  # one-shot
valen -q "explain main.go"  # TUI, runs the prompt immediately
valen -plan                 # read-only plan mode
valen -dir ~/proj           # set the working directory
valen -p "what broke?" -f error.png   # attach an image (repeat -f/--file)
valen -p "review main.go" -v          # stream thinking summaries + tool calls
valen -p "review main.go" -vv         # also raw analysis, answer text, tool results
```

Config is seeded on first run at `~/.config/valen/valen.toml` (a pre-existing
`config.toml` is still read). See
[config.toml](config.toml) for options and [SPEC.md](SPEC.md) /
[SPEC-tui.md](SPEC-tui.md) for the full spec.

## Limitations

- **Platforms:** macOS and Linux only. 
- **Providers:** OpenCode Go / OpenCode Zen and OpenRouter only.
- **Agents:** subagents run in the foreground, nest one level deep, and up to 5
  `task` calls in one model turn run in parallel. There are no background
  agents. `write`/`edit` lock per file; `shell` writes are not locked, so
  parallel tasks should work on separate files.
- **Attachments:** PNG/JPEG/GIF/WebP images ride as media parts, named with
  `Attached file: <path>`; `[attachments] auto_resize` (default on) scales
  them to `max_width`/`max_height` (2000 px) and re-encodes to fit
  `max_base64_bytes` (5 MiB). Text files are inlined opencode-style
  (`Attached file: <name>`, blank line, contents), first 2000 lines / 64 KiB
  only. Pasted screenshots need a clipboard helper: `pbpaste` (macOS),
  `wl-paste` (Wayland) or `xclip` (X11). PDF, audio and video are not
  supported. Pasted bytes are cached under `$XDG_STATE_HOME/valen/attachments/`
  for 7 days.
- **Shell safety:** the command prompts (`git push`, `rm -r`, `sudo`, ...) are a
  best-effort protection. We use macOS `sandbox-exec`, Linux `bubblewrap` for 
  file access; but like other harnesses there are limits to our exposure.

*This is early software* -- 'works for me' -- I've been hacking at it for a while. 

Review what an agent is allowed to do before pointing
it at anything you care about.

## Usage

```sh
	# run
	make
	# install
	make install
```

Tests use offline `httptest` fakes.

## License

[MIT](LICENSE)
