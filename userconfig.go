package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// defaultConfigTOML is the config shipped in the binary. First run seeds
// it into the user config dir so the binary works from any directory.
//
//go:embed config.toml
var defaultConfigTOML string

const (
	userConfigName     = "config.toml"
	userConfigOrigName = "config-ori.toml"
	userStateName      = "state.json"
	userFavouritesName = "favourites.yaml"
	userAgentsName     = "AGENTS.md"
	// maxUserAgentsBytes caps the sideloaded AGENTS.md so a huge file cannot
	// blow up every request.
	maxUserAgentsBytes = 32768
)

// userConfigDir is $XDG_CONFIG_HOME/valen, or ~/.config/valen.
func userConfigDir() (string, error) {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "valen"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "valen"), nil
}

// userAgentsSection is the user's ~/.config/valen/AGENTS.md sideloaded
// after the model's prompt file: standing instructions the model follows
// silently. Empty when the file is absent or blank.
func userAgentsSection() string {
	dir, err := userConfigDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(dir, userAgentsName))
	if err != nil {
		return ""
	}
	if len(raw) > maxUserAgentsBytes {
		raw = raw[:maxUserAgentsBytes]
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return ""
	}
	return "\n\n# User Instructions\nThe user's standing instructions follow. Apply them silently: never mention, quote or acknowledge this section or its file unless the user asks about it.\n\n" + text
}

// ensureUserConfig creates dir if needed and seeds it on first run:
// config.toml (user-owned, never overwritten) and config-ori.toml, a
// pristine copy of the shipped default (refreshed when the binary's
// default changes, so users can diff after an upgrade). It returns the
// config.toml path.
func ensureUserConfig(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	cfgPath := filepath.Join(dir, userConfigName)
	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(cfgPath, []byte(defaultConfigTOML), 0o644); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	oriPath := filepath.Join(dir, userConfigOrigName)
	if cur, err := os.ReadFile(oriPath); err != nil || !bytes.Equal(cur, []byte(defaultConfigTOML)) {
		if err := os.WriteFile(oriPath, []byte(defaultConfigTOML), 0o644); err != nil {
			return "", err
		}
	}
	return cfgPath, nil
}

// userState is the ephemeral, machine-written state kept beside the
// config (state.json). config.toml stays hand-edited; valen never rewrites
// it. Unknown keys are preserved on save.
type userState struct {
	Model    string `json:"model,omitempty"`
	Thinking string `json:"thinking,omitempty"`
}

func readStateMap(path string) map[string]any {
	m := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &m)
		if m == nil {
			m = map[string]any{}
		}
	}
	return m
}

func loadUserState(path string) userState {
	var st userState
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	return st
}

// saveModelState records the last model/thinking pair, keeping any other
// keys already in the state file. Empty thinking means provider default.
func saveModelState(path, model, thinking string) error {
	if path == "" {
		return errors.New("no state path")
	}
	m := readStateMap(path)
	m["model"] = strings.TrimSpace(model)
	if t := strings.ToLower(strings.TrimSpace(thinking)); t != "" {
		m["thinking"] = t
	} else {
		delete(m, "thinking")
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// applyUserState overlays the last-used model/thinking from the state file
// onto cfg. An invalid saved pair (model removed, level no longer valid)
// is ignored so a stale state file can never block boot.
func applyUserState(cfg Config, statePath string) Config {
	st := loadUserState(statePath)
	model := strings.TrimSpace(st.Model)
	if model == "" {
		return cfg
	}
	thinking := strings.ToLower(strings.TrimSpace(st.Thinking))
	if err := validateModelID(model); err != nil || !validThinkingLevel(model, thinking) {
		return cfg
	}
	cfg.Model = model
	cfg.Thinking = thinking
	return cfg
}

// validateModelID rejects IDs with an unknown provider prefix. Bare IDs
// ride Go; "zen/…" rides OpenCode Zen; "openrouter/…" rides OpenRouter. Anything else with a slash is
// almost certainly a pasted OpenRouter ID missing its prefix.
func validateModelID(model string) error {
	n := normalizeModelID(model)
	if strings.Contains(model, "/") && !strings.HasPrefix(n, ProviderOpenRouter+"/") && !strings.HasPrefix(n, ProviderZen+"/") {
		return fmt.Errorf("model %q has an unknown provider prefix (OpenRouter models need %q, Zen models %q)", model, ProviderOpenRouter+"/vendor/model", ProviderZen+"/model")
	}
	return nil
}

// favouritesPathFor returns the favourites file beside the state file, or ""
// when there is no state path (tests, -config <file>): favourites stay
// session-only then, like model/thinking.
func favouritesPathFor(statePath string) string {
	if statePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(statePath), userFavouritesName)
}

// parseFavouriteLine reads one YAML list item ("- glm-5.3"), tolerating
// indentation, blank lines, comments, optional quotes and inline comments.
// It returns the normalized model ID, or "" for a line to skip.
func parseFavouriteLine(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return ""
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "-"))
	if i := strings.Index(line, " #"); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if len(line) >= 2 {
		if (line[0] == '"' && line[len(line)-1] == '"') || (line[0] == '\'' && line[len(line)-1] == '\'') {
			line = line[1 : len(line)-1]
		}
	}
	return normalizeModelID(line)
}

// readFavourites loads the favourites YAML list as a set keyed by normalized
// model ID. A missing or unreadable file yields an empty set; malformed lines
// are skipped rather than blocking the picker.
func readFavourites(path string) map[string]bool {
	set := map[string]bool{}
	if path == "" {
		return set
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return set
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if id := parseFavouriteLine(line); id != "" {
			set[id] = true
		}
	}
	return set
}

// saveFavourites writes the set as a sorted YAML list, atomically, so a crash
// never leaves a truncated file.
func saveFavourites(path string, set map[string]bool) error {
	if path == "" {
		return errors.New("no favourites path")
	}
	ids := make([]string, 0, len(set))
	for id, on := range set {
		if on && strings.TrimSpace(id) != "" {
			ids = append(ids, normalizeModelID(id))
		}
	}
	sort.Strings(ids)
	var b strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&b, "- %s\n", id)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".favourites-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write([]byte(b.String())); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
