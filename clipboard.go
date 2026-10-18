package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// clipboard.go: the screenshot bridge. The terminal hands a program no image
// bytes, so one external helper reads the system clipboard; the result lands
// in a content-addressed cache file the attachment resolver can read. No cgo,
// no dependency.

const (
	clipboardTimeout = 3 * time.Second
	// attachCacheMaxAge / attachCacheMaxEntries bound the boot prune.
	attachCacheMaxAge     = 7 * 24 * time.Hour
	attachCacheMaxEntries = 200
)

// execCommand runs one clipboard helper and returns its stdout; tests stub it.
// A helper that writes more than the default attachment cap is stopped one
// byte past it, so the caller can reject it as oversize.
var execCommand = func(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), clipboardTimeout)
	defer cancel()
	var out cappedBuffer
	out.limit = defaultAttachMaxBytes + 1
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	return string(out.Bytes()), err
}

type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.limit - b.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		b.Buffer.Write(p)
	}
	return n, nil
}

// clipboardCommands lists the preferred helper first; a helper that is
// missing or writes nothing falls through to the next.
func clipboardCommands() [][]string {
	wayland := []string{"wl-paste", "--type", "image/png", "--no-newline"}
	x11 := []string{"xclip", "-selection", "clipboard", "-t", "image/png", "-o"}
	switch runtime.GOOS {
	case "darwin":
		return [][]string{{"pbpaste", "-Prefer", "png"}}
	case "linux":
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			return [][]string{wayland, x11}
		}
		return [][]string{x11, wayland}
	}
	return nil
}

// clipboardBytes returns whatever the clipboard holds: image bytes, text, or
// nothing. A non-empty notice explains a miss (and what to install on Linux).
func clipboardBytes() ([]byte, string) {
	cmds := clipboardCommands()
	if len(cmds) == 0 {
		return nil, "clipboard paste is not supported on " + runtime.GOOS
	}
	for _, c := range cmds {
		out, err := execCommand(c[0], c[1:]...)
		if err != nil || len(out) == 0 {
			continue
		}
		return []byte(out), ""
	}
	if runtime.GOOS == "linux" {
		return nil, "no clipboard image (install wl-clipboard for Wayland or xclip for X11)"
	}
	return nil, "no clipboard image"
}

// attachCacheRoot is $XDG_STATE_HOME/valen/attachments, so pasted bytes never
// leak into the session log or the workdir.
func attachCacheRoot() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), "valen-attachments")
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "valen", "attachments")
}

// cacheAttachment writes raw into the content-addressed cache (0700 dir, 0600
// file) and returns its path. Pasting the same screenshot twice costs one
// file.
func cacheAttachment(raw []byte) (string, error) {
	dir := attachCacheRoot()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	path := filepath.Join(dir, hex.EncodeToString(sum[:])+imageExt(imageMime(raw)))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// pruneAttachCache deletes cache files older than a week. One ReadDir, capped
// at attachCacheMaxEntries, at TUI boot; failures are silent.
func pruneAttachCache(now time.Time) {
	entries, err := os.ReadDir(attachCacheRoot())
	if err != nil {
		return
	}
	for i, e := range entries {
		if i >= attachCacheMaxEntries {
			return
		}
		info, err := e.Info()
		if err != nil || e.IsDir() {
			continue
		}
		if now.Sub(info.ModTime()) > attachCacheMaxAge {
			os.Remove(filepath.Join(attachCacheRoot(), e.Name()))
		}
	}
}

// attachClipboardImage reads the clipboard and caches an image result. It
// returns the cache path, the paste-as-text fallback (nil for images) and a
// user-facing notice on a miss.
func attachClipboardImage(cfg Config) (path string, text []byte, notice string) {
	raw, notice := clipboardBytes()
	if notice != "" {
		return "", nil, notice
	}
	ac := normalizeAttachmentConfig(cfg.Attachments)
	if int64(len(raw)) > ac.MaxBytes {
		return "", nil, fmt.Sprintf("clipboard image is %s, over the %s attachment cap", attachSize(int64(len(raw))), attachSize(ac.MaxBytes))
	}
	if imageMime(raw) == "" {
		return "", raw, ""
	}
	path, err := cacheAttachment(raw)
	if err != nil {
		return "", nil, "cannot cache the clipboard image: " + err.Error()
	}
	return path, nil, ""
}
