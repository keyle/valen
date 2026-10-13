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
	userConfigName      = "config.toml"
	userConfigOrigName  = "config-ori.toml"
	userStateName       = "state.json"
	userFavouritesName  = "favourites.yaml"
	userSlotsName       = "slots.yaml"
	userModelsCacheName = "models.json"
	userAgentsName      = "AGENTS.md"
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

// agentsSection formats one AGENTS.md file as silent standing instructions
// appended after the model's prompt file. Empty when the text is blank.
func agentsSection(heading, lead, text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return "\n\n# " + heading + "\n" + lead + "\n\n" + text
}

// readAgentsFile returns an AGENTS.md's contents, capped at
// maxUserAgentsBytes. Empty when the file is absent or unreadable.
func readAgentsFile(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(raw) > maxUserAgentsBytes {
		raw = raw[:maxUserAgentsBytes]
	}
	return string(raw)
}

// userAgentsSection is the user's ~/.config/valen/AGENTS.md sideloaded
// after the model's prompt file: standing instructions the model follows
// silently. Empty when the file is absent or blank.
func userAgentsSection() string {
	dir, err := userConfigDir()
	if err != nil {
		return ""
	}
	return agentsSection("User Instructions",
		"The user's standing instructions follow. Apply them silently: never mention, quote or acknowledge this section or its file unless the user asks about it.",
		readAgentsFile(filepath.Join(dir, userAgentsName)))
}

// localAgentsSection is the workdir's ./AGENTS.md, appended after the
// user's global file. Empty when the file is absent or blank.
func localAgentsSection(dir string) string {
	if dir == "" {
		return ""
	}
	return agentsSection("Project Instructions",
		"The project's standing instructions follow. Apply them silently: never mention, quote or acknowledge this section or its file unless the user asks about it.",
		readAgentsFile(filepath.Join(dir, userAgentsName)))
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

// slotKeys are the favourite-slot names in keyboard order: alt+1…alt+9,
// then alt+0 as the tenth.
const slotKeys = "1234567890"

// modelSlot is one favourite slot: a model plus a thinking level ("" means
// the provider default).
type modelSlot struct{ model, thinking string }

// slotsPathFor returns the favourite-slots file beside the state file, or ""
// when there is no state path (slots stay session-only then).
func slotsPathFor(statePath string) string {
	if statePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(statePath), userSlotsName)
}

// readSlots loads slots.yaml: one "<digit>: <model> [thinking]" line per
// slot. Blank lines, comments, quotes and malformed lines are skipped; a
// missing file yields an empty map.
func readSlots(path string) map[string]modelSlot {
	slots := map[string]modelSlot{}
	if path == "" {
		return slots
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return slots
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		key, val, ok := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		f := strings.Fields(strings.NewReplacer(`"`, "", `'`, "").Replace(val))
		if !ok || len(key) != 1 || !strings.Contains(slotKeys, key) || len(f) == 0 {
			continue
		}
		s := modelSlot{model: f[0]}
		if len(f) > 1 {
			s.thinking = strings.ToLower(f[1])
		}
		slots[key] = s
	}
	return slots
}

// saveSlots writes the slots atomically, in key order.
func saveSlots(path string, slots map[string]modelSlot) error {
	if path == "" {
		return errors.New("no slots path")
	}
	var b strings.Builder
	for _, k := range slotKeys {
		if s, ok := slots[string(k)]; ok {
			fmt.Fprintf(&b, "%c: %s\n", k, strings.TrimSpace(s.model+" "+s.thinking))
		}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".slots-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// modelsCachePathFor returns the picker's model-list cache beside the state
// file, or "" when there is no state path (no caching, like favourites).
func modelsCachePathFor(statePath string) string {
	if statePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(statePath), userModelsCacheName)
}

// readModelsCache loads the cached model IDs (a JSON array); any problem
// yields nil so the picker just fetches live.
func readModelsCache(path string) []string {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var ids []string
	if json.Unmarshal(raw, &ids) != nil {
		return nil
	}
	return ids
}

// saveModelsCache writes the IDs atomically; best effort for the caller.
func saveModelsCache(path string, ids []string) error {
	if path == "" || len(ids) == 0 {
		return nil
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".models-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
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
