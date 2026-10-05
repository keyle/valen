package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureUserConfigSeedsOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "valen")
	path, err := ensureUserConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "config.toml") {
		t.Fatalf("path=%s", path)
	}
	for _, n := range []string{"config.toml", "config-ori.toml"} {
		raw, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil || string(raw) != defaultConfigTOML {
			t.Fatalf("%s not seeded: %v", n, err)
		}
	}
	if _, err := loadConfig(path); err != nil {
		t.Fatalf("seeded config must load: %v", err)
	}
	// User edits survive; config-ori.toml stays pristine.
	if err := os.WriteFile(path, []byte("model = \"glm-5.3\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config-ori.toml"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureUserConfig(dir); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "model = \"glm-5.3\"\n" {
		t.Fatalf("user config overwritten: %q", raw)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "config-ori.toml")); string(raw) != defaultConfigTOML {
		t.Fatalf("config-ori.toml not refreshed")
	}
}

func TestUserStateRoundTripAndOverlay(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(p, []byte(`{"other":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveModelState(p, "glm-5.3", "max"); err != nil {
		t.Fatal(err)
	}
	if st := loadUserState(p); st.Model != "glm-5.3" || st.Thinking != "max" {
		t.Fatalf("state=%+v", st)
	}
	if m := readStateMap(p); m["other"] == nil {
		t.Fatalf("unknown keys must survive: %v", m)
	}
	cfg := applyUserState(Config{Model: "kimi-k2.6", Thinking: "high"}, p)
	if cfg.Model != "glm-5.3" || cfg.Thinking != "max" {
		t.Fatalf("overlay=%+v", cfg)
	}
	// Invalid saved thinking for the model is ignored.
	if err := saveModelState(p, "glm-5.3", "none"); err != nil {
		t.Fatal(err)
	}
	cfg = applyUserState(Config{Model: "kimi-k2.6", Thinking: "high"}, p)
	if cfg.Model != "kimi-k2.6" || cfg.Thinking != "high" {
		t.Fatalf("invalid state must be ignored: %+v", cfg)
	}
	// Missing file leaves cfg alone.
	cfg = applyUserState(Config{Model: "kimi-k2.6"}, filepath.Join(t.TempDir(), "nope.json"))
	if cfg.Model != "kimi-k2.6" {
		t.Fatalf("cfg=%+v", cfg)
	}
}

func TestFavouritesRoundTripAndParsing(t *testing.T) {
	dir := t.TempDir()
	// Hand-written file: indentation, comments, quotes and inline comments.
	raw := "# my favourites\n- \"openrouter/anthropic/claude-opus-5.5\"\n  - glm-5.3  # Go\n\n"
	p := filepath.Join(dir, userFavouritesName)
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readFavourites(p)
	if !got["glm-5.3"] || !got["openrouter/anthropic/claude-opus-5.5"] || len(got) != 2 {
		t.Fatalf("parsed favourites=%v", got)
	}
	// Save is sorted and round-trips; an unset entry is dropped.
	got["kimi-k3"] = true
	got["glm-5.3"] = false
	if err := saveFavourites(p, got); err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(p)
	want := "- kimi-k3\n- openrouter/anthropic/claude-opus-5.5\n"
	if string(saved) != want {
		t.Fatalf("saved=%q want=%q", saved, want)
	}
	if got := readFavourites(p); !got["kimi-k3"] || got["glm-5.3"] || len(got) != 2 {
		t.Fatalf("reloaded=%v", got)
	}
	// Missing file and empty path are empty, not errors.
	if got := readFavourites(filepath.Join(dir, "nope.yaml")); len(got) != 0 {
		t.Fatalf("missing file=%v", got)
	}
	if got := readFavourites(""); len(got) != 0 {
		t.Fatalf("empty path=%v", got)
	}
	if p := favouritesPathFor(""); p != "" {
		t.Fatalf("no state path must mean session-only, got %q", p)
	}
	if p := favouritesPathFor(filepath.Join(dir, "state.json")); p != filepath.Join(dir, userFavouritesName) {
		t.Fatalf("favourites path=%q", p)
	}
}
