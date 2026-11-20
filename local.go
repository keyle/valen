package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/keyle/valen/internal/dvlog"
)

const (
	localConfigName  = "local.toml"
	defaultLocalHost = "127.0.0.1"
	defaultLocalPort = 8080
)

// LocalConfig mirrors ~/.config/valen/local.toml: local MLX backends, works
// like valen.toml but for `local/` models. One [[models]] entry per model.
// Sampling pointers preserve valen's omit-means-server-default semantics, so
// unset fields let mlx-vlm resolve the model's generation_config.json.
type LocalConfig struct {
	Models []LocalModel `toml:"models"`
}

type LocalModel struct {
	Name                        string   `toml:"name"`
	Path                        string   `toml:"path"`
	Backend                     string   `toml:"backend"`
	Port                        int      `toml:"port"`
	BaseURL                     string   `toml:"base_url"`
	EnableThinking              *bool    `toml:"enable_thinking"`
	ThinkingBudget              int      `toml:"thinking_budget"`
	MaxTokens                   int      `toml:"max_tokens"`
	Temperature                 *float32 `toml:"temperature"`
	TopP                        *float32 `toml:"top_p"`
	TopK                        *int     `toml:"top_k"`
	RepetitionPenalty           *float32 `toml:"repetition_penalty"`
	CompactionTemperature       *float32 `toml:"compaction_temperature"`
	CompactionTopP              *float32 `toml:"compaction_top_p"`
	CompactionTopK              *int     `toml:"compaction_top_k"`
	CompactionRepetitionPenalty *float32 `toml:"compaction_repetition_penalty"`
	ThinkingFamily              string   `toml:"thinking_family"`
	ThinkingLevels              []string `toml:"thinking_levels"`
	MaxContextTokens            int      `toml:"max_context_tokens"`
	ExtraArgs                   []string `toml:"extra_args"`
}

// localConfigPath is ~/.config/valen/local.toml beside valen.toml.
// VALEN_LOCAL_CONFIG overrides it (tests and -config pinned runs).
func localConfigPath() string {
	if v := strings.TrimSpace(os.Getenv("VALEN_LOCAL_CONFIG")); v != "" {
		return expandHome(v)
	}
	dir, err := userConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, localConfigName)
}

// loadLocalConfig reads local.toml; missing file yields empty config.
func loadLocalConfig(path string) (LocalConfig, error) {
	var cfg LocalConfig
	if path == "" {
		return cfg, nil
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("stat %s: %w", path, err)
	}
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return cfg, fmt.Errorf("decode %s: %w", path, err)
	}
	if bad := md.Undecoded(); len(bad) > 0 {
		names := make([]string, len(bad))
		for i, k := range bad {
			names[i] = k.String()
		}
		return cfg, fmt.Errorf("decode %s: unknown key(s): %s", path, strings.Join(names, ", "))
	}
	for i := range cfg.Models {
		m := &cfg.Models[i]
		m.Name = strings.TrimSpace(m.Name)
		m.Path = strings.TrimSpace(m.Path)
		m.Backend = strings.ToLower(strings.TrimSpace(m.Backend))
		m.BaseURL = strings.TrimSpace(m.BaseURL)
		m.ThinkingFamily = strings.ToLower(strings.TrimSpace(m.ThinkingFamily))
		if m.Backend == "" {
			m.Backend = "mlx-vlm"
		}
		if m.Backend != "mlx-vlm" && m.Backend != "openai-compatible" {
			return cfg, fmt.Errorf("local model %q: backend %q must be mlx-vlm or openai-compatible", m.Name, m.Backend)
		}
		if m.Name == "" {
			return cfg, fmt.Errorf("local model at %s: name is required", m.Path)
		}
		if strings.Contains(m.Name, "/") || strings.ContainsAny(m.Name, " \t\n") {
			return cfg, fmt.Errorf("local model %q: name must not contain / or whitespace (stored as local/<name>)", m.Name)
		}
		if m.ThinkingFamily != "" && m.ThinkingFamily != "toggle" && m.ThinkingFamily != "effort" {
			return cfg, fmt.Errorf("local model %q: thinking_family %q must be toggle or effort", m.Name, m.ThinkingFamily)
		}
		if m.EnableThinking == nil {
			t := true
			m.EnableThinking = &t
		}
	}
	return cfg, nil
}

// expandHome expands ~/ and $HOME-style leading ~ in a path.
func expandHome(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// localModelByName finds a model by picker name (suffix after local/).
func localModelByName(cfg LocalConfig, name string) (LocalModel, bool) {
	n := strings.TrimSpace(name)
	for _, m := range cfg.Models {
		if strings.EqualFold(m.Name, n) {
			return m, true
		}
	}
	return LocalModel{}, false
}

// localModelForID resolves a stored local/<name> ID to its entry.
func localModelForID(id string) (LocalModel, bool) {
	if modelProvider(id) != ProviderLocal {
		return LocalModel{}, false
	}
	base := wireModelID(id)
	cfg, _ := loadLocalConfig(localConfigPath())
	return localModelByName(cfg, base)
}

// localModelIDs lists stored local/<name> IDs from local.toml plus any model
// dirs found under cfg.LocalModelsDir that are not already listed. Each dir
// directly under local_models_dir containing config.json is an implicit
// model named by its folder name.
func localModelIDs(localCfg LocalConfig, valenCfg Config) []string {
	seen := make(map[string]bool)
	var ids []string
	for _, m := range localCfg.Models {
		stored := ProviderLocal + "/" + m.Name
		key := strings.ToLower(stored)
		if !seen[key] {
			seen[key] = true
			ids = append(ids, stored)
		}
	}
	for _, root := range localModelsDirs(valenCfg) {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(root, e.Name())
			if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
				continue
			}
			stored := ProviderLocal + "/" + e.Name()
			if seen[strings.ToLower(stored)] {
				continue
			}
			seen[strings.ToLower(stored)] = true
			ids = append(ids, stored)
		}
	}
	sort.Strings(ids)
	return ids
}

// localModelsDirs expands valen.toml local_models_dir (string) to dirs.
func localModelsDirs(cfg Config) []string {
	if strings.TrimSpace(cfg.LocalModelsDir) == "" {
		return nil
	}
	return []string{expandHome(cfg.LocalModelsDir)}
}

// ensureLocalModel creates an implicit entry for a scanned dir.
func ensureLocalModel(name, dir string) LocalModel {
	t := true
	return LocalModel{Name: name, Path: dir, Backend: "mlx-vlm", EnableThinking: &t}
}

// resolveLocalModel loads the entry for a stored ID, synthesising one from
// local_models_dir when local.toml has no explicit entry.
func resolveLocalModel(id string) (LocalModel, bool) {
	if modelProvider(id) != ProviderLocal {
		return LocalModel{}, false
	}
	base := wireModelID(id)
	lcfg, _ := loadLocalConfig(localConfigPath())
	if m, ok := localModelByName(lcfg, base); ok {
		return m, true
	}
	// Implicit: folder name under local_models_dir.
	// Config is loaded per call so tests can point LocalModelsDir at temp dirs.
	// Fall back to the last loaded valen config when available via env override
	// for tests: use LOCAL_MODELS_DIR env when no file config exists.
	for _, root := range implicitRoots() {
		dir := filepath.Join(root, base)
		if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
			return ensureLocalModel(base, dir), true
		}
		// Case-insensitive folder match.
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if strings.EqualFold(e.Name(), base) {
				d := filepath.Join(root, e.Name())
				if _, err := os.Stat(filepath.Join(d, "config.json")); err == nil {
					return ensureLocalModel(e.Name(), d), true
				}
			}
		}
	}
	return LocalModel{}, false
}

var testLocalModelsDir string

func implicitRoots() []string {
	if testLocalModelsDir != "" {
		return []string{expandHome(testLocalModelsDir)}
	}
	if v := strings.TrimSpace(os.Getenv("VALEN_LOCAL_MODELS_DIR")); v != "" {
		return []string{expandHome(v)}
	}
	// Default scan root mirrors the documented default (~/Code/models).
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, "Code", "models")}
}

// localBaseURL returns the OpenAI base URL for a local model, starting the
// mlx-vlm server when needed.
func localBaseURL(m LocalModel) (string, error) {
	if m.Backend == "openai-compatible" {
		if m.BaseURL != "" {
			return strings.TrimRight(m.BaseURL, "/"), nil
		}
		port := m.Port
		if port <= 0 {
			port = defaultLocalPort
		}
		return fmt.Sprintf("http://127.0.0.1:%d/v1", port), nil
	}
	return ensureLocalServer(m)
}

type runningServer struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	base string
	port int
}

var localServers = struct {
	sync.Mutex
	byID map[string]*runningServer
}{byID: map[string]*runningServer{}}

func freePort() int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return defaultLocalPort
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// ensureLocalServer starts (or reuses) the mlx_vlm.server for m and returns
// its OpenAI base URL (http://127.0.0.1:<port>/v1).
func ensureLocalServer(m LocalModel) (string, error) {
	key := strings.ToLower(m.Name)
	localServers.Lock()
	if s, ok := localServers.byID[key]; ok && s.cmd != nil && s.cmd.Process != nil {
		if s.cmd.ProcessState == nil {
			base := s.base
			localServers.Unlock()
			return base, nil
		}
	}
	localServers.Unlock()
	port := m.Port
	if port <= 0 {
		port = freePort()
	}
	m.Port = port
	path := expandHome(m.Path)
	if path == "" {
		return "", fmt.Errorf("local model %q: path is required", m.Name)
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("local model %q: path %s: %w", m.Name, path, err)
	}
	base := fmt.Sprintf("http://127.0.0.1:%d/v1", port)
	args := []string{"-m", "mlx_vlm.server", "--model", path, "--host", defaultLocalHost, "--port", fmt.Sprint(port)}
	if m.EnableThinking != nil && *m.EnableThinking {
		args = append(args, "--enable-thinking")
	}
	if m.ThinkingBudget > 0 {
		args = append(args, "--thinking-budget", fmt.Sprint(m.ThinkingBudget))
	}
	if m.MaxTokens > 0 {
		args = append(args, "--max-tokens", fmt.Sprint(m.MaxTokens))
	}
	args = append(args, m.ExtraArgs...)
	py := localPython(m)
	cmd := exec.Command(py, args...)
	cmd.Stdout = localLogWriter(m.Name, "stdout")
	cmd.Stderr = localLogWriter(m.Name, "stderr")
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("local model %q: start mlx_vlm.server (%s %s): %w", m.Name, py, strings.Join(args, " "), err)
	}
	s := &runningServer{cmd: cmd, base: base, port: port}
	dvlog.Logf("local %s: spawned %s %s → %s (pid %d)", m.Name, py, strings.Join(args, " "), base, cmd.Process.Pid)
	go reapLocalServer(m.Name, s)
	if err := waitLocalHealth(cmd, base, 60*time.Second); err != nil {
		_ = cmd.Process.Kill()
		return "", fmt.Errorf("local model %q: %w", m.Name, err)
	}
	localServers.Lock()
	if existing, ok := localServers.byID[key]; ok && existing.cmd != nil && existing.cmd.ProcessState == nil {
		localServers.Unlock()
		_ = cmd.Process.Kill()
		return existing.base, nil
	}
	localServers.byID[key] = s
	localServers.Unlock()
	return base, nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// localPython picks the interpreter for mlx_vlm.server: MLX_VLM_PYTHON wins,
// then a .venv beside the model path (e.g. ~/Code/models/.venv), then python3.
func localPython(m LocalModel) string {
	if v := strings.TrimSpace(os.Getenv("MLX_VLM_PYTHON")); v != "" {
		return v
	}
	path := expandHome(m.Path)
	if path != "" {
		for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
			cand := filepath.Join(dir, ".venv", "bin", "python")
			if _, err := os.Stat(cand); err == nil {
				return cand
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
		}
	}
	for _, root := range implicitRoots() {
		cand := filepath.Join(root, ".venv", "bin", "python")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return "python3"
}

func reapLocalServer(name string, s *runningServer) {
	_ = s.cmd.Wait()
	dvlog.Logf("local %s: server exited", name)
}

func waitLocalHealth(cmd *exec.Cmd, base string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	// base is .../v1; health lives beside it at /health.
	health := strings.TrimSuffix(base, "/v1") + "/health"
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if cmd != nil && cmd.ProcessState != nil {
			return fmt.Errorf("server exited early: %s (GET %s)", cmd.ProcessState, health)
		}
		req, err := http.NewRequest(http.MethodGet, health, nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				if resp.StatusCode == 200 {
					// /health is 200 while the model still loads; wait for ready.
					var h struct {
						ModelReady *bool `json:"model_ready"`
					}
					if json.Unmarshal(data, &h) == nil && h.ModelReady != nil {
						if *h.ModelReady {
							return nil
						}
					} else {
						return nil
					}
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("server not healthy after %s (GET %s)", timeout, health)
}

// stopLocalServers kills every spawned mlx-vlm server. Called on shutdown.
func stopLocalServers() {
	localServers.Lock()
	defer localServers.Unlock()
	for k, s := range localServers.byID {
		if s.cmd != nil && s.cmd.Process != nil && s.cmd.ProcessState == nil {
			_ = s.cmd.Process.Kill()
			dvlog.Logf("local %s: stopped server pid %d", k, s.cmd.Process.Pid)
		}
		delete(localServers.byID, k)
	}
}

type localLog struct {
	model, stream string
}

func (l localLog) Write(p []byte) (int, error) {
	dvlog.Logf("local %s %s: %s", l.model, l.stream, strings.TrimSpace(string(p)))
	return len(p), nil
}

func localLogWriter(model, stream string) io.Writer {
	return localLog{model: model, stream: stream}
}

// localThinkingLevels returns the levels for a local model from local.toml,
// defaulting to toggle none/high when thinking is on.
func localThinkingLevels(m LocalModel) []string {
	if len(m.ThinkingLevels) > 0 {
		return append([]string(nil), m.ThinkingLevels...)
	}
	if m.ThinkingFamily == "effort" {
		return append([]string(nil), standardThinkingLevels...)
	}
	// toggle default: harness wants reasoning, so high on by default.
	return []string{"none", "high"}
}

// localThinkingFamily reports toggle/effort for a local model.
func localThinkingFamily(m LocalModel, modelID string) string {
	if m.ThinkingFamily != "" {
		return m.ThinkingFamily
	}
	// Fall back to family inference on the stored ID.
	if thinkingFamily(modelID) == "toggle" {
		return "toggle"
	}
	// LFM3 and other local reasoning models are on/off, not effort levels.
	l := strings.ToLower(m.Name)
	if strings.Contains(l, "lfm") {
		return "toggle"
	}
	return "effort"
}

// localEnableThinking resolves the request-level flag: cfg thinking wins
// (none→false, any other named level→true), empty falls back to the
// local.toml default (true for the harness).
func localEnableThinking(m LocalModel, thinking string) bool {
	thinking = strings.ToLower(strings.TrimSpace(thinking))
	if thinking != "" {
		return thinking != "none"
	}
	if m.EnableThinking != nil {
		return *m.EnableThinking
	}
	return true
}
