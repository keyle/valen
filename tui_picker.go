package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sashabaranov/go-openai"
)

// openModelPicker fetches the live catalogs off the UI thread: Go plus
// OpenRouter (models arrive stored with their provider prefix). Both are
// listed even without keys — fallbacks cover unreachable catalogs — so a
// missing key surfaces on pick/send, never as a boot crash. With a filter,
// an exact or unique-substring match switches directly; otherwise the
// filtered picker opens.
func (m *tuiModel) openModelPicker(filter string) tea.Cmd {
	m.notice = "Loading models…"
	m.noticeErr = false
	m.modelFilter = filter
	m.modelLoadFilter = filter
	m.modelSearch.SetValue(filter)
	// Cached list: open instantly from disk and refresh live after a debounce
	// (see modelsRefreshMsg), since most opens just switch between known models.
	if cached := readModelsCache(modelsCachePathFor(m.statePath)); len(cached) > 0 {
		return tea.Batch(
			func() tea.Msg { return modelsLoadedMsg{ids: cached, filter: filter} },
			tea.Tick(modelsRefreshDelay, func(time.Time) tea.Msg { return modelsRefreshMsg{filter: filter} }),
		)
	}
	return m.fetchModelsCmd(filter, false)
}

// fetchModelsCmd fetches the live catalogs off the UI thread and, when the Go
// list succeeded, stores them in the cache. refresh marks a background update
// of an open picker.
func (m *tuiModel) fetchModelsCmd(filter string, refresh bool) tea.Cmd {
	cfg := m.cfg
	goKey := effectiveOpenCodeKey(cfg, m.apiKey)
	orKey := effectiveOpenRouterKey(cfg)
	cachePath := modelsCachePathFor(m.statePath)
	client := m.client
	if client == nil && goKey != "" {
		client = newClient(cfg, goKey)
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var ids []string
		var err error
		if client != nil {
			ids, err = fetchGoModels(ctx, client)
		} else {
			err = fmt.Errorf("no client")
		}
		if err != nil || len(ids) == 0 {
			ids = append([]string(nil), fallbackGoModels...)
		}
		zenIDs, zenErr := fetchZenModels(ctx, nil, zenBaseURL, goKey)
		if zenErr != nil || len(zenIDs) == 0 {
			zenIDs = prefixedZenModels(fallbackZenModels)
		}
		ids = append(ids, zenIDs...)
		orIDs, orPrices, orErr := fetchOpenRouterCatalog(ctx, nil, openRouterBaseURL, orKey)
		if orErr != nil || len(orIDs) == 0 {
			orIDs = prefixedOpenRouterFallback()
			orPrices = nil
		}
		ids = append(ids, orIDs...)
		if err == nil {
			_ = saveModelsCache(cachePath, ids)
		}
		return modelsLoadedMsg{ids: ids, err: err, filter: filter, pricing: orPrices, refresh: refresh}
	}
}

// applyModelsRefresh swaps a freshly fetched catalog into the open picker,
// keeping the search text and the cursor on the same model.
func (m *tuiModel) applyModelsRefresh(ids []string) {
	var cursorID string
	if m.cursor >= 0 && m.cursor < len(m.modelChoices) {
		cursorID = m.modelChoices[m.cursor].id
	}
	ids = append([]string(nil), ids...)
	sort.Strings(ids)
	m.modelCatalog = m.modelCatalog[:0]
	for _, id := range ids {
		m.modelCatalog = append(m.modelCatalog, modelChoice{id: id, current: id == m.cfg.Model, transport: modelTransport(id)})
	}
	m.modelFilter = m.modelSearch.Value()
	m.filterModelChoices()
	for i, c := range m.modelChoices {
		if c.id == cursorID {
			m.cursor = i
			break
		}
	}
	if m.cursor >= len(m.modelChoices) {
		m.cursor = max(len(m.modelChoices)-1, 0)
	}
	m.refreshViewport()
	m.ensureCursorVisible()
}

// filterModelChoices rebuilds the visible model rows from the full catalog and
// the current search text. Keeping the catalog intact lets clearing a query
// reveal models hidden by earlier filtering.
func (m *tuiModel) filterModelChoices() {
	ids := make([]string, 0, len(m.modelCatalog))
	byID := make(map[string]modelChoice, len(m.modelCatalog))
	for _, choice := range m.modelCatalog {
		ids = append(ids, choice.id)
		byID[choice.id] = choice
	}
	matched := matchGoModel(ids, m.modelFilter)
	m.modelChoices = m.modelChoices[:0]
	for _, id := range matched {
		m.modelChoices = append(m.modelChoices, byID[id])
	}
	// Starred models float to the top, keeping catalog (alphabetical) order
	// within each group so a toggle never shuffles the rest of the list.
	sort.SliceStable(m.modelChoices, func(i, j int) bool {
		fi, fj := m.isFavourite(m.modelChoices[i].id), m.isFavourite(m.modelChoices[j].id)
		return fi && !fj
	})
}

// favouritesPath is the picker's favourites file, "" when there is no state
// path (session-only, like model/thinking).
func (m *tuiModel) favouritesPath() string { return favouritesPathFor(m.statePath) }

func (m *tuiModel) isFavourite(id string) bool {
	return m.favourites[normalizeModelID(id)]
}

// toggleFavourite stars or unstars the highlighted model, persists the set
// and re-sorts the list, keeping the cursor on the same model. Space drives
// it, focused or not (the filter never needs a space); Enter stays selection.
func (m *tuiModel) toggleFavourite() {
	if m.cursor < 0 || m.cursor >= len(m.modelChoices) {
		return
	}
	if m.favourites == nil {
		m.favourites = readFavourites(m.favouritesPath())
	}
	id := m.modelChoices[m.cursor].id
	key := normalizeModelID(id)
	added := !m.favourites[key]
	if added {
		m.favourites[key] = true
	} else {
		delete(m.favourites, key)
	}
	label := displayModelID(id)
	suffix := ""
	if path := m.favouritesPath(); path != "" {
		if err := saveFavourites(path, m.favourites); err != nil {
			m.notice = "Favourites save failed: " + err.Error()
			m.noticeErr = true
			m.refreshViewport()
			return
		}
	} else {
		suffix = " (session only)"
	}
	if added {
		m.notice = "★ " + label + " added to favourites." + suffix
	} else {
		m.notice = label + " removed from favourites." + suffix
	}
	m.noticeErr = false
	m.modelFilter = m.modelSearch.Value()
	m.filterModelChoices()
	for i, c := range m.modelChoices {
		if c.id == id {
			m.cursor = i
			break
		}
	}
	m.refreshViewport()
	m.ensureCursorVisible()
}

// slotShifted are the US-layout symbols alt+shift+<digit> produces, in
// slotKeys order (1…9, 0). Terminals send the shifted character, not the digit.
const slotShifted = "!@#$%^&*()"

// slotKey maps a key string to its favourite slot ("1"…"0"): alt+<digit>
// picks it, alt+shift+<digit> (alt+!…alt+)) assigns it.
func slotKey(s string) (key string, assign, ok bool) {
	r, found := strings.CutPrefix(s, "alt+")
	if !found || len(r) != 1 {
		return "", false, false
	}
	if strings.Contains(slotKeys, r) {
		return r, false, true
	}
	if i := strings.Index(slotShifted, r); i >= 0 {
		return slotKeys[i : i+1], true, true
	}
	return "", false, false
}

func (m *tuiModel) loadSlots() {
	if m.slots == nil {
		m.slots = readSlots(slotsPathFor(m.statePath))
	}
}

// assignSlot stores the current model + thinking level in slot key and
// announces it. Persists to slots.yaml; session-only without a state path.
func (m *tuiModel) assignSlot(key string) {
	m.loadSlots()
	m.slots[key] = modelSlot{model: m.cfg.Model, thinking: m.cfg.Thinking, routing: m.cfg.Routing}
	m.notice = fmt.Sprintf("Slot %s ← %s · %s%s.", key, displayModelID(m.cfg.Model), effortLabel(m.cfg.Thinking), slotRoutingSuffix(m.cfg.Routing))
	m.noticeErr = false
	if path := slotsPathFor(m.statePath); path == "" {
		m.notice += " (session only)"
	} else if err := saveSlots(path, m.slots); err != nil {
		m.notice = "Slot save failed: " + err.Error()
		m.noticeErr = true
	}
}

// unbindSelectedSlot clears the slot at the slots-view cursor and persists it.
func (m *tuiModel) unbindSelectedSlot() {
	m.loadSlots()
	if m.cursor < 0 || m.cursor >= len(slotKeys) {
		return
	}
	key := string(slotKeys[m.cursor])
	if _, ok := m.slots[key]; !ok {
		m.notice = fmt.Sprintf("Slot %s is already empty.", key)
		m.noticeErr = false
		m.refreshViewport()
		return
	}
	delete(m.slots, key)
	m.notice = fmt.Sprintf("Slot %s unbound.", key)
	m.noticeErr = false
	if path := slotsPathFor(m.statePath); path == "" {
		m.notice += " (session only)"
	} else if err := saveSlots(path, m.slots); err != nil {
		m.notice = "Slot save failed: " + err.Error()
		m.noticeErr = true
	}
	m.refreshViewport()
}

// applySlot switches to the model + thinking level stored in slot key.
func (m *tuiModel) applySlot(key string) tea.Cmd {
	m.loadSlots()
	s, ok := m.slots[key]
	if !ok {
		m.notice = fmt.Sprintf("Slot %s is empty — set a model + thinking, then alt+shift+%s assigns it.", key, key)
		m.noticeErr = true
		return nil
	}
	if err := validateModelID(s.model); err != nil {
		m.notice = fmt.Sprintf("Slot %s: %v", key, err)
		m.noticeErr = true
		return nil
	}
	m.cfg.Thinking = s.thinking // applyModelSwitch resets it if the model rejects it
	m.cfg.Routing = normalizeRouting(s.routing)
	m.applyModelSwitch(s.model)
	if !m.noticeErr {
		m.notice = fmt.Sprintf("Slot %s: %s · %s%s", key, displayModelID(m.cfg.Model), effortLabel(m.cfg.Thinking), slotRoutingSuffix(m.cfg.Routing))
		if m.cfg.Thinking != s.thinking {
			m.notice += fmt.Sprintf(" (%s unavailable)", s.thinking)
		}
	}
	return m.fetchQuotaCmd()
}

// slotRoutingSuffix names a slot's routing in a notice, empty when none.
func slotRoutingSuffix(routing string) string {
	if r := normalizeRouting(routing); r != "" {
		return " · " + r
	}
	return ""
}

// openEffortPicker lists the thinking levels available on the current model,
// plus the provider default.
func (m *tuiModel) openEffortPicker() {
	m.effortLevels = append([]string{""}, AvailableThinkingLevels(m.cfg.Model)...)
	m.show(viewEffort)
	for i, l := range m.effortLevels {
		if l == m.cfg.Thinking {
			m.cursor = i
		}
	}
	m.refreshViewport()
}

// applyModelSwitch moves the session to id, resets an incompatible thinking
// level, and persists both to state.json so the choice sticks. Unknown
// models probe the provider in the background: a bogus effort 400s with the
// valid list, which is cached on disk for ctrl+t. A model whose provider key
// is missing still switches (so keyless boots can browse), but the notice
// says which key is missing instead of pretending the switch is ready.
func (m *tuiModel) applyModelSwitch(id string) {
	prevThinking := m.cfg.Thinking
	m.cfg.Model = id
	if len(m.history) > 0 && m.history[0].Role == openai.ChatMessageRoleSystem {
		m.history[0].Content = effectiveSystem(m.cfg)
	} else {
		m.history = append(seedHistory(m.cfg), m.history...)
	}
	resetNote := ""
	if !validThinkingLevel(id, m.cfg.Thinking) {
		m.cfg.Thinking = ""
		if prevThinking != "" {
			resetNote = fmt.Sprintf(" Thinking reset to default (%s is not available on %s).", prevThinking, id)
		}
	}
	msg := fmt.Sprintf("Switched to %s.%s", id, resetNote)
	if m.statePath != "" {
		if err := saveModelState(m.statePath, m.cfg.Model, m.cfg.Thinking, m.cfg.Routing); err != nil {
			m.notice = msg + " (state save failed: " + err.Error() + ")"
			m.noticeErr = true
		} else {
			m.notice = msg + " Saved."
			m.noticeErr = false
		}
	} else {
		m.notice = msg + " (session only)"
		m.noticeErr = false
	}
	if err := missingKeyError(m.cfg, m.apiKey); err != nil {
		if modelProvider(id) == ProviderOpenRouter {
			m.notice = fmt.Sprintf("Switched to %s. OpenRouter key not found — export %s=... to use it.", id, openRouterKeyEnv)
		} else {
			m.notice = fmt.Sprintf("Switched to %s. OpenCode API key not found — set %s=... to use it.", id, openCodeKeyEnv)
		}
		m.noticeErr = true
	}
	m.show(viewChat)
	if _, ok := cachedThinkingLevels(id); !ok {
		if _, ok := lookupKnownLevels(modelBaseID(id)); !ok && m.httpClient != nil && missingKeyError(m.cfg, m.apiKey) == nil {
			cfg, apiKey, hc := m.cfg, m.apiKey, m.httpClient
			base := m.ctx
			if base == nil {
				base = context.Background()
			}
			go func() {
				ctx, cancel := context.WithTimeout(base, 15*time.Second)
				defer cancel()
				_, _ = probeThinkingLevels(ctx, cfg, apiKey, hc)
			}()
		}
	}
}

// setThinkingLevel validates, persists and announces level, preserving the
// current view mode. It reports whether the level changed.
func (m *tuiModel) setThinkingLevel(level string) bool {
	level = strings.ToLower(strings.TrimSpace(level))
	if !validThinkingLevel(m.cfg.Model, level) {
		m.notice = fmt.Sprintf("Thinking %q is not available on %s (try: %s).", level, m.cfg.Model, strings.Join(AvailableThinkingLevels(m.cfg.Model), ", "))
		m.noticeErr = true
		return false
	}
	m.cfg.Thinking = level
	display := level
	if display == "" {
		display = "provider default"
	}
	msg := fmt.Sprintf("Thinking set to %s on %s.", display, m.cfg.Model)
	if m.statePath != "" {
		if err := saveModelState(m.statePath, m.cfg.Model, m.cfg.Thinking, m.cfg.Routing); err != nil {
			m.notice = msg + " (state save failed: " + err.Error() + ")"
			m.noticeErr = true
		} else {
			m.notice = msg + " Saved."
			m.noticeErr = false
		}
	} else {
		m.notice = msg + " (session only)"
		m.noticeErr = false
	}
	return true
}

// applyThinkingSwitch sets the reasoning level for the current model,
// or clears it back to the provider default. "default", "auto" and "off"
// all clear.
func (m *tuiModel) applyThinkingSwitch(level string) {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "default" || level == "auto" || level == "off" || level == "-" {
		level = ""
	}
	if !m.setThinkingLevel(level) {
		return
	}
	m.show(viewChat)
}

// cycleThinking moves the thinking level forward low to high through
// AvailableThinkingLevels, wrapping back to the lowest. ctrl+t is the
// mnemonic (T-hinking); ctrl+] / ctrl+[ never arrive reliably in the
// terminal (ctrl+[ is esc), so a single wrapping key beats up/down.
func (m *tuiModel) cycleThinking() {
	levels := AvailableThinkingLevels(m.cfg.Model)
	if len(levels) == 0 {
		m.notice = "No adjustable thinking on " + m.cfg.Model + " (provider default only)."
		m.noticeErr = false
		m.refreshViewport()
		return
	}
	idx := -1
	for i, l := range levels {
		if l == m.cfg.Thinking {
			idx = i
			break
		}
	}
	if len(levels) == 1 && idx == 0 {
		m.notice = "Thinking only offers " + effortLabel(levels[0]) + " on " + m.cfg.Model + "."
		m.noticeErr = false
		m.refreshViewport()
		return
	}
	next := 0
	if idx >= 0 {
		next = (idx + 1) % len(levels)
	}
	if m.setThinkingLevel(levels[next]) {
		m.refreshViewport()
	}
}

// setRouting validates and persists the OpenRouter provider-routing mode,
// announcing the result. It reports whether the routing applies (OpenRouter
// model, valid mode). On Go/Zen it is a silent no-op: no notice, no write.
func (m *tuiModel) setRouting(routing string) bool {
	if modelProvider(m.cfg.Model) != ProviderOpenRouter {
		return false
	}
	r := normalizeRouting(routing)
	if !validRouting(r) {
		m.notice = fmt.Sprintf("Routing %q is not valid (try: none, floor, nitro, exacto).", routing)
		m.noticeErr = true
		return false
	}
	m.cfg.Routing = r
	msg := fmt.Sprintf("Routing set to %s on %s.", routingLabel(r), displayModelID(m.cfg.Model))
	if m.statePath != "" {
		if err := saveModelState(m.statePath, m.cfg.Model, m.cfg.Thinking, m.cfg.Routing); err != nil {
			m.notice = msg + " (state save failed: " + err.Error() + ")"
			m.noticeErr = true
		} else {
			m.notice = msg + " Saved."
			m.noticeErr = false
		}
	} else {
		m.notice = msg + " (session only)"
		m.noticeErr = false
	}
	return true
}

// cycleRouting advances the OpenRouter routing mode through
// none → floor → nitro → exacto → none. ctrl+y is the mnemonic. On Go/Zen it
// is a silent no-op (no notice, no state write).
func (m *tuiModel) cycleRouting() {
	if modelProvider(m.cfg.Model) != ProviderOpenRouter {
		return
	}
	m.setRouting(cycleRouting(m.cfg.Routing))
}

// applyRouteSwitch sets the routing mode from a /route argument.
func (m *tuiModel) applyRouteSwitch(arg string) {
	if !m.setRouting(arg) {
		return
	}
	m.show(viewChat)
}
