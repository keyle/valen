# Change: attach-input

Composer input for files and screenshots: paste an image's bytes, drop a
file on the terminal, and the model gets it — without typing a path.
Builds on `attachments/` (resolver + transport parts); this change is the
input layer only.

## Why

`attachments` answers "how does a file reach the model". The missing half is
"how does a file reach valen" without a path: screenshots live on the
clipboard, drag-and-drop hands the terminal a path. Typing `@~/Desktop/
Screenshot\ 2026-10-18\ at\ 10.12.44.png` is the friction we want gone.

## What changes

1. Token syntax in the chatbox: `[img-1]` for a clipboard image,
   `[shot.png]` for a dropped/pasted file. Tokens are plain text and stay
   visible to the model; they mark where the attachment was referenced.
2. Paste path: a bracketed paste that is one or more existing file paths
   (drag-and-drop in iTerm/Terminal/Ghostty/WezTerm/foot) becomes one chip per
   file instead of typed text; non-path pastes behave exactly as today.
3. Clipboard image path: `ctrl+v` (and `alt+v`) probes the OS clipboard for
   PNG/JPEG bytes through a `pbpaste`/`wl-paste`/`xclip` bridge (no Go dep),
   writes them to a private cache, and inserts `[img-N]`.
4. A paste that yields neither text nor an image shows one notice
   ("clipboard has no image; drop the file or use -f"), never a silent no-op.
5. Composer shows the pending list above the chatbox: name, KiB, WxH. Tokens
   that were deleted from the text are reconciled out at submit.
6. `-p`/`-q` keep `-f` from `attachments`; `[img-1]` in headless text stays
   literal (no composer state to resolve against).

## Non-goals

- Inline image rendering in the terminal; clipboard text history; arbitrary
  clipboard formats (HTML, RTF, `x-special/…` file lists); attaching images
  mid-turn from a tool result.

## Success criteria

- Offline tests with a stubbed clipboard command (`execCommand` var):
  paste-path → chip, clipboard PNG → chip, no clipboard → notice, deleted
  token dropped at submit, image part present in the request body.
  Terminal-dependent paths are documented, not tested.

## Feasibility

Yes, all three paths are local and offline: drag-and-drop already arrives as
bracketed-paste text, and the clipboard image is one external
`pbpaste`/`wl-paste`/`xclip` call (no Go dep). No terminal image protocol,
no OSC 1337/kitty graphics, no cgo. The only honest caveat is terminal
variety: some terminals send nothing on an image paste, so `ctrl+v` and
`-f` remain the dependable routes, and every failure is a visible notice.
