package main

import (
	"errors"
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
	if path != filepath.Join(dir, "valen.toml") {
		t.Fatalf("path=%s", path)
	}
	for _, n := range []string{"valen.toml", "config-ori.toml"} {
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

func TestEnsureUserConfigFallsBackToLegacy(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(legacy, []byte("model = \"glm-5.3\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := ensureUserConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if path != legacy {
		t.Fatalf("path=%s want legacy", path)
	}
	if _, err := os.Stat(filepath.Join(dir, "valen.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("valen.toml must not be seeded beside a legacy config: %v", err)
	}
	if raw, _ := os.ReadFile(legacy); string(raw) != "model = \"glm-5.3\"\n" {
		t.Fatalf("legacy config overwritten: %q", raw)
	}
}

func TestUserStateRoundTripAndOverlay(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(p, []byte(`{"other":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveModelState(p, "glm-5.3", "max", ""); err != nil {
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
	if err := saveModelState(p, "glm-5.3", "none", ""); err != nil {
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

func TestRoutingStateRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := saveModelState(p, "openrouter/z-ai/glm-5.3", "high", "nitro"); err != nil {
		t.Fatal(err)
	}
	st := loadUserState(p)
	if st.Model != "openrouter/z-ai/glm-5.3" || st.Thinking != "high" || st.Routing != "nitro" {
		t.Fatalf("state=%+v", st)
	}
	cfg := applyUserState(Config{Model: "kimi-k2.6", Thinking: "high"}, p)
	if cfg.Model != "openrouter/z-ai/glm-5.3" || cfg.Thinking != "high" || cfg.Routing != "nitro" {
		t.Fatalf("overlay=%+v", cfg)
	}
	// "none" is stored as absence (empty).
	if err := saveModelState(p, "openrouter/z-ai/glm-5.3", "high", "none"); err != nil {
		t.Fatal(err)
	}
	if st := loadUserState(p); st.Routing != "" {
		t.Fatalf("none routing must round-trip empty, got %q", st.Routing)
	}
	// A stale/invalid saved routing is ignored without blocking the model.
	if err := os.WriteFile(p, []byte(`{"model":"openrouter/z-ai/glm-5.3","thinking":"high","routing":"bogus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg = applyUserState(Config{Model: "kimi-k2.6"}, p)
	if cfg.Model != "openrouter/z-ai/glm-5.3" || cfg.Routing != "" {
		t.Fatalf("invalid routing must be ignored: %+v", cfg)
	}
}

func TestSlotsRoutingRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), userSlotsName)
	raw := "1: openrouter/moonshotai/kimi-k3 high floor\n2: glm-5.3 none\n3: openrouter/anthropic/claude-opus-5.5 nitro\n"
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readSlots(p)
	if got["1"] != (modelSlot{model: "openrouter/moonshotai/kimi-k3", thinking: "high", routing: "floor"}) {
		t.Fatalf("slot 1=%+v", got["1"])
	}
	// "none" is a thinking level, not routing: it must land in thinking.
	if got["2"] != (modelSlot{model: "glm-5.3", thinking: "none"}) {
		t.Fatalf("slot 2=%+v", got["2"])
	}
	if got["3"] != (modelSlot{model: "openrouter/anthropic/claude-opus-5.5", routing: "nitro"}) {
		t.Fatalf("slot 3=%+v", got["3"])
	}
	if err := saveSlots(p, got); err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(p)
	want := "1: openrouter/moonshotai/kimi-k3 high floor\n2: glm-5.3 none\n3: openrouter/anthropic/claude-opus-5.5 nitro\n"
	if string(saved) != want {
		t.Fatalf("saved=%q want=%q", saved, want)
	}
}
