package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sashabaranov/go-openai"
)

// runningToolCount reports how many live tool calls have started but not
// finished yet — the open threads.
func (m *tuiModel) runningToolCount() int {
	n := 0
	for _, t := range m.pendingTools {
		if !t.done {
			n++
		}
	}
	return n
}

// finishPendingTool marks the in-flight tool call done, matched by call ID
// so parallel same-name calls keep their own outputs. Empty IDs fall back to
// the latest name match (older event sources).
func (m *tuiModel) finishPendingTool(id, name, output string, durMs int64) {
	if id != "" {
		for i := range m.pendingTools {
			if m.pendingTools[i].id == id && !m.pendingTools[i].done {
				m.pendingTools[i].done = true
				m.pendingTools[i].output = output
				m.pendingTools[i].durMs = durMs
				m.pendingTools[i].renderOK = false
				return
			}
		}
	}
	for i := len(m.pendingTools) - 1; i >= 0; i-- {
		if m.pendingTools[i].name == name && !m.pendingTools[i].done {
			m.pendingTools[i].done = true
			m.pendingTools[i].output = output
			m.pendingTools[i].durMs = durMs
			m.pendingTools[i].renderOK = false
			return
		}
	}
	m.pendingTools = append(m.pendingTools, pendingTool{name: name, output: output, done: true, durMs: durMs})
}

// flushStream re-renders the live transcript, throttled so a flood of deltas
// stays cheap. The committed turnDoneMsg always renders the final state.
func (m *tuiModel) flushStream() {
	if time.Since(m.lastStreamFlush) < 60*time.Millisecond {
		return
	}
	m.lastStreamFlush = time.Now()
	m.refreshViewport()
	if m.mode == viewChat && m.follow {
		m.viewport.GotoBottom()
	}
}

func (m *tuiModel) submit(text string) tea.Cmd {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	if strings.EqualFold(trimmed, ":q") {
		return tea.Quit // vim-style alias for /quit
	}
	if strings.HasPrefix(trimmed, "/") {
		return m.handleCommand(trimmed)
	}
	if m.busy {
		m.notice = "Working… press esc twice to cancel."
		m.noticeErr = false
		return nil
	}
	// Refresh from the live env so an export after boot is picked up, then
	// fail fast with a notice (not a crash) when the current model has no key.
	m.apiKey = effectiveOpenCodeKey(m.cfg, m.apiKey)
	m.cfg.OpenRouterKey = effectiveOpenRouterKey(m.cfg)
	m.cfg.OpenAIKey = effectiveOpenAIKey(m.cfg)
	if err := missingKeyError(m.cfg, m.apiKey); err != nil {
		switch modelProvider(m.cfg.Model) {
		case ProviderOpenRouter:
			m.notice = fmt.Sprintf("OpenRouter key not found — export %s=... to use %s.", openRouterKeyEnv, m.cfg.Model)
		case ProviderOpenAI:
			m.notice = fmt.Sprintf("OpenAI key not found — export %s=... to use %s.", openAIKeyEnv, m.cfg.Model)
		default:
			m.notice = fmt.Sprintf("OpenCode API key not found — set %s=... to use %s.", openCodeKeyEnv, m.cfg.Model)
		}
		m.noticeErr = true
		m.refreshViewport()
		return nil
	}
	if m.retrying {
		m.rewindLastTurn()
	}
	// Chips whose token left the text are dropped; the rest resolve to paths
	// off the UI thread (folder approval may prompt).
	attachPaths := m.reconcileAttachments(trimmed)
	histBase := append([]openai.ChatCompletionMessage(nil), m.history...)

	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	// Fresh buffer per turn: anything steered into the previous turn has
	// either been delivered or moved back to the queue in turnDoneMsg.
	m.steer = &steerQueue{}
	ctx, cancel := context.WithCancel(base)
	ctx = withSteerQueue(ctx, m.steer)
	m.cancel = cancel
	m.busy = true
	m.pendingSteers = nil
	m.pendingPrompt = trimmed
	m.pendingPlan = m.cfg.Plan
	m.session.nameSession(trimmed)
	m.pendingSince = time.Now()
	m.askSince = time.Time{}
	m.toolStatus = "Sending…"
	m.roundCur, m.roundMax = 0, 0
	m.pendingTools = nil
	m.pendingText = ""
	m.pendingThinking = ""
	m.pendingThinkIdx = 0
	m.orMeta = nil
	m.pendingCost, m.pendingCostSet = 0, false
	m.lastStreamFlush = time.Time{}
	m.notice = ""
	m.done = make(chan struct{})
	m.pinBottom()

	cfg := m.cfg
	client := m.client
	apiKey := m.apiKey
	httpClient := m.httpClient
	registry := m.registry
	prog := m.prog
	return func() tea.Msg {
		refs, notices := resolveAttachments(ctx, attachPaths, cfg)
		if len(notices) > 0 && prog != nil {
			prog.Send(noticeMsg{text: strings.Join(notices, " · ")})
		}
		prompt := trimmed
		if len(refs) > 0 {
			prompt = promptWithAttachments(trimmed, refs)
			ctx = withTurnAttachments(ctx, refs)
		}
		h := append(histBase, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: prompt})
		var tools []ToolRecord
		var steers []Steer
		var thinking, answer string
		var orMeta *ORMeta
		var turnCost float64
		var hasTurnCost bool
		thinkIdx := 0
		var roundAnswer, roundThinking string
		onEvent := func(ev Event) {
			switch ev.Kind {
			case "round_start":
				answer = roundBreak(answer)
				thinking = roundBreak(thinking)
				roundAnswer, roundThinking = answer, thinking
			case "retry":
				// The failed attempt's partial output is replayed by the
				// next attempt; drop it so nothing shows twice.
				answer, thinking = roundAnswer, roundThinking
			case "user_steer":
				steers = append(steers, Steer{Text: ev.Text, Plan: cfg.Plan})
			case "tool_start":
				tools = append(tools, ToolRecord{ID: ev.ID, Name: ev.Text, Args: ev.Args})
			case "tool_result":
				for i := len(tools) - 1; i >= 0; i-- {
					if tools[i].ID == ev.ID && tools[i].Name == ev.Text {
						tools[i].Output = ev.Output
						tools[i].DurMs = ev.DurMs
						break
					}
				}
			case "text_delta":
				answer = appendStreamText(answer, ev.Text)
			case "thinking_delta":
				thinking, thinkIdx = appendThinkingDelta(thinking, ev.Text, ev.Index, thinkIdx)
			case "or_meta":
				orMeta = mergeORMeta(orMeta, ev.ORMeta)
				if ev.ORMeta != nil && ev.ORMeta.HasCost {
					turnCost += ev.ORMeta.Cost
					hasTurnCost = true
				}
			}
			if prog != nil {
				prog.Send(toolEventMsg{kind: ev.Kind, text: ev.Text, args: ev.Args, output: ev.Output, id: ev.ID, durMs: ev.DurMs, idx: ev.Index, orMeta: ev.ORMeta})
			}
		}
		reply, usage, err := runTurn(ctx, cfg, client, apiKey, httpClient, registry, &h, onEvent)
		if err == nil {
			streamedThinking := thinking
			thinking, answer = splitThinking(reply.Content)
			if reply.ReasoningContent != "" {
				thinking = reply.ReasoningContent
			}
			// The streamed buffer carries the part boundaries the live
			// view rendered (a newline between summary parts); the
			// authoritative reply may concatenate fragments exactly, so
			// prefer the streamed buffer for display when deltas
			// arrived. Empty means the non-streaming fallback (or
			// <think> in content only) with nothing to preserve.
			if streamedThinking != "" {
				thinking = streamedThinking
			}
		}
		return turnDoneMsg{prompt: trimmed, thinking: thinking, answer: answer, tools: tools, steers: steers, usage: usage, err: err, history: h, model: cfg.Model, effort: cfg.Thinking, plan: cfg.Plan, orMeta: orMeta, turnCost: turnCost, hasTurnCost: hasTurnCost}
	}
}

// handleCtrlEnter queues the chatbox text for the next turn while the agent
// is working; when idle it is just another enter.
func (m *tuiModel) handleCtrlEnter() (tea.Model, tea.Cmd) {
	if !m.busy {
		return m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	}
	if text := strings.TrimSpace(m.input.Value()); text != "" {
		m.queuePrompt(text)
	}
	return m, nil
}

// steerInput hands the chatbox text to the running turn. It lands between
// rounds, never mid-stream; attachments cannot ride a running turn, so those
// queue for the next one.
func (m *tuiModel) steerInput(text string) {
	if len(m.pendingForText()) > 0 || m.steer == nil || !m.steer.push(steerItem{text: text, plan: m.pendingPlan}) {
		m.queuePrompt(text)
		return
	}
	m.input.SetValue("")
	m.closeFilePicker()
	m.notice = "Steering — lands after the current step."
	m.noticeErr = false
	m.pinBottom()
}

// queuePrompt appends a message to the next-turn queue and clears the
// composer, keeping the mode and chips it was typed in.
func (m *tuiModel) queuePrompt(text string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return
	}
	if len(m.queued) >= steerCap {
		m.notice = fmt.Sprintf("Queue full (%d) — send it when this turn ends.", steerCap)
		m.noticeErr = true
		return
	}
	m.queued = append(m.queued, queuedPrompt{text: trimmed, plan: m.cfg.Plan, attach: m.pendingForText(), imgSeq: m.imgSeq})
	m.input.SetValue("")
	m.clearAttachments()
	m.closeFilePicker()
	m.queueParked = false
	m.notice = fmt.Sprintf("Queued (%d) — runs after this turn.", len(m.queued))
	m.noticeErr = false
	m.pinBottom()
}

// startNextQueued starts the oldest queued prompt as a normal turn, restoring
// its mode and chips first.
func (m *tuiModel) startNextQueued() tea.Cmd {
	if len(m.queued) == 0 {
		return nil
	}
	q := m.queued[0]
	m.queued = m.queued[1:]
	m.queueParked = false
	m.applyPlanMode(q.plan)
	m.input.SetValue(q.text)
	m.input.CursorEnd()
	m.pendingAttachments = q.attach
	m.imgSeq = q.imgSeq
	return m.submit(q.text)
}

// applyPlanMode toggles plan mode at runtime: flips the flag, reseeds the
// system message so the next turn carries the plan overlay (or drops it),
// and notices which way we went. History after index 0 is untouched.
func (m *tuiModel) applyPlanMode(on bool) {
	m.cfg.Plan = on
	if len(m.history) > 0 && m.history[0].Role == openai.ChatMessageRoleSystem {
		m.history[0].Content = effectiveSystem(m.cfg)
	} else {
		m.history = seedHistory(m.cfg)
	}
	m.input.Placeholder = inputPlaceholder(on)
	if on {
		m.notice = "Plan Mode enabled: read only and planning"
	} else {
		m.notice = "Work mode enabled"
	}
	m.noticeErr = false
	if m.busy {
		// The running turn already captured its config and gates.
		m.notice += " Applies from your next message."
	}
	m.refreshViewport()
}

func (m *tuiModel) handleCommand(text string) tea.Cmd {
	fields := strings.Fields(text)
	cmd := strings.ToLower(fields[0])
	switch cmd {
	case "/clear":
		m.history = seedHistory(m.cfg)
		m.clearAttachments()
		resetTodos()
		m.lastTurnDur = 0
		m.pendingCost, m.pendingCostSet = 0, false
		if m.session != nil {
			m.session.Turns = nil
			m.session.TotalUsage = Usage{}
		}
		m.notice = "Cleared — fresh conversation."
		m.noticeErr = false
		m.show(viewChat)
		return nil
	case "/new":
		m.startFreshSession()
		return nil
	case "/think":
		m.showThink = !m.showThink
		if m.showThink {
			m.notice = "Thinking blocks shown."
		} else {
			m.notice = "Thinking blocks hidden."
		}
		m.noticeErr = false
		m.refreshViewport()
		return nil
	case "/save":
		msg, isErr := m.exportTranscript()
		m.notice = msg
		m.noticeErr = isErr
		return nil
	case "/copy":
		m.copyLastReply()
		return nil
	case "/help", "/h", "/?":
		m.show(viewHelp)
		return nil
	case "/history":
		m.show(viewHistory)
		return nil
	case "/sessions", "/resume", "/continue":
		m.show(viewSessions)
		return nil
	case "/tools":
		m.show(viewTools)
		return nil
	case "/slots":
		m.show(viewSlots)
		return nil
	case "/errors":
		m.show(viewErrors)
		return nil
	case "/models":
		if len(fields) == 1 {
			return m.openModelPicker("")
		}
		arg := fields[1]
		if knownGoModel(arg) {
			m.applyModelSwitch(matchGoModel(fallbackGoModels, arg)[0])
			return m.fetchQuotaCmd()
		}
		return m.openModelPicker(arg)
	case "/effort", "/thinking":
		if len(fields) == 1 {
			m.openEffortPicker()
			return nil
		}
		m.applyThinkingSwitch(fields[1])
		return nil
	case "/route", "/routing":
		if modelProvider(m.cfg.Model) != ProviderOpenRouter {
			m.notice = "Routing applies to OpenRouter models only (none/floor/nitro/exacto)."
			m.noticeErr = false
			return nil
		}
		if len(fields) == 1 {
			m.notice = fmt.Sprintf("Routing is %s on %s (none/floor/nitro/exacto; ctrl+y cycles).", routingLabel(m.cfg.Routing), displayModelID(m.cfg.Model))
			m.noticeErr = false
			return nil
		}
		m.applyRouteSwitch(fields[1])
		return nil
	case "/compact", "/summarize":
		return m.compactCmd()
	case "/plan":
		m.applyPlanMode(!m.cfg.Plan)
		return nil
	case "/allow-all":
		on := !allowAllEnabled()
		if len(fields) > 1 {
			switch strings.ToLower(fields[1]) {
			case "on", "true", "1", "yes":
				on = true
			case "off", "false", "0", "no":
				on = false
			default:
				m.notice = "Usage: /allow-all [on|off]"
				m.noticeErr = true
				return nil
			}
		}
		setAllowAll(on)
		if on {
			m.notice = "Allow-all ON: file, folder, network and command prompts are skipped this session (deny rules and .env secrets still apply)."
		} else {
			m.notice = "Allow-all off: permission prompts are back."
		}
		m.noticeErr = false
		return nil
	case "/quit", "/exit", "/q":
		return tea.Quit
	default:
		m.notice = fmt.Sprintf("Unknown command %s — try /help.", fields[0])
		m.noticeErr = true
		return nil
	}
}

// Update drives the Bubble Tea loop. Single in-process runtime, no server.
func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	mdl, cmd := m.update(msg)
	m.syncInputHeight()
	return mdl, cmd
}

func (m *tuiModel) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	// Any real interaction (key, click, wheel, refocus) means the user is
	// back: stop the repeat bell. Background messages (stream deltas,
	// timers) must not.
	switch msg.(type) {
	case tea.KeyMsg, tea.MouseMsg, tea.FocusMsg:
		m.attentionUntil = time.Time{}
	}

	// Disambiguated ctrl+m (the letter form terminals send once a
	// disambiguating mode is on) opens the model picker from anywhere an
	// overlay question is not waiting for its keys.
	if ctrlMCSI(msg) {
		if m.ask != nil {
			return m, nil
		}
		return m, m.openModelPicker("")
	}

	// Kitty disambiguate mode reports esc and ctrl/alt+key as CSI-u, which
	// bubbletea drops as unknown: translate back into ordinary key events.
	if km, ok := csiUKey(msg); ok {
		return m.Update(km)
	}

	// CSI-u / modifyOtherKeys enter: shift+enter (and friends) that legacy
	// terminals collapse into plain enter. mod 1 is unmodified enter, so it
	// submits like the enter key; any modifier inserts a newline instead.
	if mod, ok := enterCSIMod(msg); ok {
		if m.ask != nil || m.mode != viewChat {
			return m, nil
		}
		if mod == 1 {
			return m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
		}
		// ctrl+enter queues for the next turn while the agent is working;
		// idle it has nothing to queue for, so it behaves like enter.
		if mod == 5 {
			return m.handleCtrlEnter()
		}
		m.insertInputNewline()
		return m, nil
	}

	switch msg := msg.(type) {
	case mouseHoldFlushMsg:
		// Ambiguous fragments that never completed a mouse report were
		// typed text: release them. A completed report is already
		// dropped at arrival, so anything still held here is typing.
		if msg.gen == m.mouseHoldGen {
			m.flushMouseHold()
		}
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		// Chrome around the transcript: header, status, footer, the input
		// border plus a line of slack (6) beside the configured box lines.
		// Pickers hide the chatbox and footer, so viewportHeight reclaims
		// that space for the scrollback.
		vpW := m.columnWidth() - 4
		if vpW < 40 {
			vpW = 40
		}
		m.viewport.Width = vpW
		m.viewport.Height = m.viewportHeight()
		m.input.SetWidth(vpW - 2)
		m.glam = nil
		m.refreshViewport()
		if m.follow && m.mode != viewAsk {
			m.viewport.GotoBottom()
		}
		return m, nil

	case spinner.TickMsg:
		if m.busy {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
			// Only the status-line spinner animates on the tick; the scrollback
			// is refreshed by flushStream on real content changes, so a tick no
			// longer re-renders (and re-lexes) the whole pending block.
		}
		return m, tea.Batch(cmds...)

	case toolEventMsg:
		switch msg.kind {
		case "round_start":
			m.toolStatus = "Reasoning…"
			if cur, max, ok := parseRoundProgress(msg.text); ok {
				m.roundCur, m.roundMax = cur, max
			}
			m.pendingText = roundBreak(m.pendingText)
			m.pendingThinking = roundBreak(m.pendingThinking)
			m.roundText, m.roundThinking = m.pendingText, m.pendingThinking
		case "retry":
			m.pendingText, m.pendingThinking = m.roundText, m.roundThinking
			m.toolStatus = msg.text
			if msg.args != "" || msg.output != "" {
				m.retryErrs = append(m.retryErrs, errEntry{at: time.Now(), title: "Retry: " + msg.args, details: msg.output})
				if len(m.retryErrs) > 50 {
					m.retryErrs = m.retryErrs[1:]
				}
			}
		case "user_steer":
			m.pendingSteers = append(m.pendingSteers, Steer{Text: msg.text, Plan: m.pendingPlan})
			m.toolStatus = "steering…"
		case "tool_start":
			m.pendingTools = append(m.pendingTools, pendingTool{name: msg.text, args: msg.args, id: msg.id})
			if n := m.runningToolCount(); n > 1 {
				m.toolStatus = fmt.Sprintf("%s… · %d running", msg.text, n)
			} else {
				m.toolStatus = msg.text + "…"
			}
		case "tool_result":
			m.finishPendingTool(msg.id, msg.text, msg.output, msg.durMs)
			if n := m.runningToolCount(); n > 0 {
				m.toolStatus = fmt.Sprintf("✓ %s · %d still running", msg.text, n)
			} else {
				m.toolStatus = "✓ " + msg.text
			}
		case "text_delta":
			m.pendingText = appendStreamLine(m.pendingText, msg.text)
			m.toolStatus = "writing…"
			m.flushStream()
			return m, nil
		case "thinking_delta":
			m.pendingThinking, m.pendingThinkIdx = appendThinkingDelta(m.pendingThinking, msg.text, msg.idx, m.pendingThinkIdx)
			m.toolStatus = "Reasoning…"
			m.flushStream()
			return m, nil
		case "or_meta":
			if msg.orMeta == nil {
				return m, nil
			}
			m.orMeta = mergeORMeta(m.orMeta, msg.orMeta)
			if msg.orMeta.HasCost {
				m.pendingCost += msg.orMeta.Cost
				m.pendingCostSet = true
			}
			return m, nil
		default:
			m.toolStatus = msg.text
		}
		// Tool events share the delta throttle: a parallel batch emits 2N
		// events at once and the committed turnDoneMsg always renders final
		// state, so collapsing a burst into one render loses nothing.
		m.flushStream()
		return m, nil

	case usageMsg:
		m.usageInflight = false
		if msg.err == nil {
			u := msg.usage
			m.goUsage = &u
		}
		return m, nil

	case orCreditsMsg:
		m.orCreditsInflight = false
		if msg.err == nil {
			c := msg.credits
			m.orCredits = &c
		}
		return m, nil

	case orPricingMsg:
		if len(msg.pricing) > 0 {
			if m.orPricing == nil {
				m.orPricing = map[string]orPricing{}
			}
			for id, p := range msg.pricing {
				m.orPricing[id] = p
			}
		}
		return m, nil

	case escExpireMsg:
		if m.escArmedAt.Equal(msg.at) {
			m.escArmedAt = time.Time{}
			if m.busy {
				m.notice = "Working… press esc twice to cancel."
				m.noticeErr = false
			}
		}
		return m, nil
	case usageTickMsg:
		return m, tea.Batch(m.fetchQuotaCmd(), usageTick())

	case fileIndexMsg:
		if msg.root != getFileRoot() {
			// Workdir changed under the walk: drop the stale index so the
			// next keystroke starts a fresh one.
			m.fileIndex, m.fileIndexReady, m.fileIndexLoading = nil, false, false
			return m, nil
		}
		m.fileIndex = msg.entries
		m.fileIndexReady = true
		m.fileIndexTrunc = msg.truncated
		m.fileIndexLoading = false
		m.fileIndexAt = time.Now()
		if m.files.open {
			m.filterFileChoices()
			m.syncFilePickerHeight()
		}
		return m, nil

	case modelsRefreshMsg:
		if m.mode != viewModels || msg.filter != m.modelLoadFilter {
			return m, nil // picker closed or replaced; the next open reschedules
		}
		return m, m.fetchModelsCmd(msg.filter, true)

	case modelsLoadedMsg:
		if msg.filter != m.modelLoadFilter {
			return m, nil // stale fetch; a newer picker request is in flight
		}
		if msg.pricing != nil {
			m.orPricing = msg.pricing
		}
		if msg.refresh {
			// Background update: only a still-open picker, only live data.
			if m.mode == viewModels && msg.err == nil && len(msg.ids) > 0 {
				m.applyModelsRefresh(msg.ids)
			}
			return m, nil
		}
		ids := msg.ids
		if msg.err != nil || len(ids) == 0 {
			// Go live list failed: keep any non-Go rows already merged (live
			// or fallback) and backfill Go with the curated list, so keyless
			// boots still browse every configured provider.
			var orKeep, zenKeep, aiKeep []string
			for _, id := range msg.ids {
				switch modelProvider(id) {
				case ProviderOpenRouter:
					orKeep = append(orKeep, id)
				case ProviderZen:
					zenKeep = append(zenKeep, id)
				case ProviderOpenAI:
					aiKeep = append(aiKeep, id)
				}
			}
			if len(orKeep) == 0 {
				orKeep = prefixedOpenRouterFallback()
			}
			if len(zenKeep) == 0 {
				zenKeep = prefixedZenModels(fallbackZenModels)
			}
			if len(aiKeep) == 0 && effectiveOpenAIKey(m.cfg) != "" {
				aiKeep = prefixedOpenAIModels(fallbackOpenAIModels)
			}
			ids = append(append([]string(nil), fallbackGoModels...), zenKeep...)
			ids = append(ids, aiKeep...)
			ids = append(ids, orKeep...)
			m.notice = "Live model list unreachable — showing known models."
			m.noticeErr = false
		} else {
			m.notice = ""
		}
		if strings.TrimSpace(msg.filter) != "" {
			matched := matchGoModel(ids, msg.filter)
			switch len(matched) {
			case 0:
				m.notice = fmt.Sprintf("No models match %q.", msg.filter)
				m.noticeErr = true
				return m, nil
			case 1:
				m.applyModelSwitch(matched[0])
				return m, tea.Batch(m.fetchQuotaCmd(), m.fetchPricingCmd())
			}
		}
		sort.Strings(ids)
		m.modelCatalog = m.modelCatalog[:0]
		for _, id := range ids {
			m.modelCatalog = append(m.modelCatalog, modelChoice{id: id, current: id == m.cfg.Model, transport: modelTransport(id)})
		}
		m.modelSearch.SetValue(msg.filter)
		m.modelFilter = msg.filter
		if path := m.favouritesPath(); path != "" {
			m.favourites = readFavourites(path)
		}
		if m.favourites == nil {
			m.favourites = map[string]bool{}
		}
		m.filterModelChoices()
		m.show(viewModels)
		for i, c := range m.modelChoices {
			if c.current {
				m.cursor = i
			}
		}
		m.refreshViewport()
		return m, nil

	case noticeMsg:
		m.notice = msg.text
		m.noticeErr = false
		m.refreshViewport()
		return m, nil

	case clipboardPasteMsg:
		switch {
		case msg.notice != "":
			m.notice = msg.notice
			m.noticeErr = false
		case msg.path != "":
			token := m.nextImageToken()
			m.addPendingAttachment(token, msg.path)
			m.input.InsertString(token + " ")
			m.dismissTransientNotice()
		case msg.text != "":
			m.insertClipboardText(msg.text)
		default:
			m.notice = "Nothing to paste."
			m.noticeErr = false
		}
		m.refreshViewport()
		return m, m.syncFilePicker()

	case questionMsg:
		picks := make([]map[int]bool, len(msg.qs))
		for i := range msg.qs {
			picks[i] = map[int]bool{}
		}
		m.ask = &askState{qs: msg.qs, reply: msg.reply, picks: picks, ownText: make([]string, len(msg.qs)), phase: "pick"}
		m.closeFilePicker()
		m.mode = viewAsk
		if m.askSince.IsZero() {
			m.askSince = time.Now()
		}
		m.input.Blur()
		m.viewport.Height = m.viewportHeight() // overlay hides the chatbox
		m.refreshViewport()
		m.viewport.GotoTop()
		return m, m.startAttention()

	case turnDoneMsg:
		turn := Turn{
			At: msg.promptAt(), Prompt: msg.prompt, Thinking: msg.thinking, Reply: msg.answer, Tools: msg.tools, Steers: msg.steers,
			Usage: Usage{Prompt: msg.usage.PromptTokens, Completion: msg.usage.CompletionTokens, Total: msg.usage.TotalTokens},
			Model: msg.model, Effort: msg.effort, Plan: msg.plan, History: msg.history,
			ORMeta: msg.orMeta,
		}
		if msg.hasTurnCost {
			turn.Cost = msg.turnCost
		}
		if p, ok := m.orPricing[msg.model]; ok {
			turn.InPrice, turn.OutPrice = p.prompt, p.completion
		}
		if !m.pendingSince.IsZero() {
			m.resumeTimer()
			turn.DurMs = time.Since(m.pendingSince).Milliseconds()
			if turn.DurMs < 1 {
				turn.DurMs = 1
			}
			m.lastTurnDur = time.Duration(turn.DurMs) * time.Millisecond
		}
		if msg.err != nil {
			fe := friendlyError(msg.err)
			turn.Error = &fe
			if isCancel(msg.err) {
				m.notice = "Cancelled — resend when ready."
				m.noticeErr = false
			} else {
				m.notice = fe.Title
				m.noticeErr = true
			}
		} else {
			m.notice = ""
		}
		m.history = append([]openai.ChatCompletionMessage(nil), msg.history...)
		// Model/plan switches during a request apply to the next request, not
		// the completed turn's attribution or checkpoint.
		if len(m.history) > 0 && m.history[0].Role == openai.ChatMessageRoleSystem {
			m.history[0].Content = effectiveSystem(m.cfg)
		}
		m.busy = false
		// The turn may have written files: refresh the picker index lazily.
		m.fileIndexAt = time.Time{}
		fcmd := m.syncFilePicker()
		m.pendingPrompt = ""
		m.orMeta = msg.orMeta
		if msg.hasTurnCost {
			m.orSpent += msg.turnCost
			m.orSpentSet = true
		}
		m.pendingCost, m.pendingCostSet = 0, false
		m.toolStatus = ""
		m.roundCur, m.roundMax = 0, 0
		m.pendingTools = nil
		m.pendingSteers = nil
		m.pendingText = ""
		m.pendingThinking = ""
		m.pendingThinkIdx = 0
		m.cancel = nil
		// Steers typed after the last round boundary never reached the model:
		// keep them, in order, ahead of the messages queued for later.
		if left := m.steer.drain(); len(left) > 0 {
			items := make([]queuedPrompt, 0, len(left))
			for _, st := range left {
				items = append(items, queuedPrompt{text: st.text, plan: st.plan})
			}
			m.queued = append(items, m.queued...)
		}
		m.steer = &steerQueue{}
		if msg.err != nil {
			// A failed or cancelled turn parks the queue: keep it visible,
			// run nothing until the user sends again.
			m.queueParked = len(m.queued) > 0
			if isCancel(msg.err) && len(m.queued) > 0 {
				m.notice = fmt.Sprintf("Cancelled — %d queued · enter runs the next one.", len(m.queued))
				m.noticeErr = false
			}
		}
		if m.session != nil {
			if err := m.session.append(turn); err != nil {
				m.notice = "Session save failed: " + err.Error()
				m.noticeErr = true
			} else {
				m.session.ensureMeta(msg.model, msg.effort)
			}
		}
		if m.done != nil {
			close(m.done)
			m.done = nil
		}
		m.input.Focus()
		m.pinBottom()
		if msg.err != nil && isCancel(msg.err) {
			// The user stopped it themselves: no bell.
			return m, tea.Batch(fcmd, m.fetchQuotaCmd())
		}
		cmds := []tea.Cmd{fcmd, m.fetchQuotaCmd(), m.startAttention()}
		// A clean finish with an empty chatbox runs the next queued message;
		// a half-typed draft is never overwritten.
		if msg.err == nil && !m.queueParked && len(m.queued) > 0 && strings.TrimSpace(m.input.Value()) == "" {
			cmds = append(cmds, m.startNextQueued())
		}
		return m, tea.Batch(cmds...)

	case compactDoneMsg:
		m.busy = false
		m.toolStatus = ""
		m.cancel = nil
		if msg.err != nil {
			if isCancel(msg.err) {
				m.notice = "Compaction cancelled."
				m.noticeErr = false
			} else {
				m.notice = "Compaction failed: " + friendlyError(msg.err).Title
				m.noticeErr = true
			}
		} else if msg.compacted {
			m.history = msg.history
			m.notice = fmt.Sprintf("Compacted %d→%d tokens (summary by %s). Disk log untouched.", msg.before, msg.after, msg.model)
			m.noticeErr = false
		} else if msg.pruned > 0 {
			m.history = msg.history
			m.notice = fmt.Sprintf("Pruned older tool output %d→%d tokens; nothing left to summarize.", msg.before, msg.after)
			m.noticeErr = false
		} else {
			m.notice = "Nothing to compact — history already small."
			m.noticeErr = false
		}
		m.input.Focus()
		m.pinBottom()
		return m, nil

	case tea.MouseMsg:
		if m.copyClicked(msg) {
			m.copyLastReply()
			return m, nil
		}
		if m.mode == viewChat {
			if handled, cmd := m.handleMouseSelection(msg); handled {
				return m, cmd
			}
		}
		// Wheel (and clicks) go to the viewport in every mode: in chat
		// the textarea would swallow them and the scrollback would never
		// move. Scrolling away from the bottom pauses autoscroll;
		// scrolling back to the bottom resumes it. Any stale selection is
		// dropped: its line indexes would no longer line up.
		m.clearSelection()
		var vcmd tea.Cmd
		m.viewport, vcmd = m.viewport.Update(msg)
		if m.mode == viewChat {
			m.follow = m.viewport.AtBottom()
		}
		if vcmd != nil {
			cmds = append(cmds, vcmd)
		}
		return m, tea.Batch(cmds...)

	case attentionPingMsg:
		return m, m.repeatAttention(msg)

	case selScrollMsg:
		if !m.selAtScrollEdge() || !m.selScrollStep() {
			m.selScrollTicking = false
			return m, nil
		}
		return m, m.selScrollTick()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Forward anything else (e.g. textarea blink) to focused widgets.
	if m.mode == viewChat {
		var icmd tea.Cmd
		m.input, icmd = m.input.Update(msg)
		if icmd != nil {
			cmds = append(cmds, icmd)
		}
	} else if m.mode == viewModels {
		var scmd tea.Cmd
		m.modelSearch, scmd = m.modelSearch.Update(msg)
		if scmd != nil {
			cmds = append(cmds, scmd)
		}
	} else {
		var vcmd tea.Cmd
		m.viewport, vcmd = m.viewport.Update(msg)
		if vcmd != nil {
			cmds = append(cmds, vcmd)
		}
	}
	return m, tea.Batch(cmds...)
}

func (msg turnDoneMsg) promptAt() (t time.Time) {
	return time.Now()
}

func isCancel(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "cancel") || strings.Contains(s, "context canceled")
}

func (m *tuiModel) openSelected() tea.Cmd {
	switch m.mode {
	case viewModels:
		if m.cursor < 0 || m.cursor >= len(m.modelChoices) {
			return nil
		}
		m.applyModelSwitch(m.modelChoices[m.cursor].id)
		return tea.Batch(m.fetchQuotaCmd(), m.fetchPricingCmd())
	case viewEffort:
		if m.cursor < 0 || m.cursor >= len(m.effortLevels) {
			return nil
		}
		m.applyThinkingSwitch(m.effortLevels[m.cursor])
	case viewHistory:
		items := m.historyItems()
		if m.cursor < 0 || m.cursor >= len(items) {
			return nil
		}
		it := items[m.cursor]
		if it.isFile {
			m.detailTitle = "Session " + filepath.Base(it.path)
			m.detailBody = renderSessionFileText(m, it.path)
		} else if it.turnIdx >= 0 && it.turnIdx < len(m.session.Turns) {
			t := m.session.Turns[it.turnIdx]
			m.detailTitle = fmt.Sprintf("Turn %d · %s", it.turnIdx+1, t.At.Format("15:04:05"))
			m.detailBody = m.renderTurnFull(&t)
		}
		m.show(viewDetail)
	case viewTools:
		items := m.toolItems()
		if m.cursor < 0 || m.cursor >= len(items) {
			return nil
		}
		r := items[m.cursor].record
		m.detailTitle = "Tool " + r.Name
		m.detailBody = fmt.Sprintf("args:\n%s\n\noutput:\n%s", r.Args, r.Output)
		m.show(viewDetail)
	case viewSessions:
		items := m.sessionItems()
		if m.cursor < 0 || m.cursor >= len(items) {
			return nil
		}
		m.resumeSessionPath(items[m.cursor].path)
	case viewSlots:
		if m.cursor < 0 || m.cursor >= len(slotKeys) {
			return nil
		}
		return m.applySlot(string(slotKeys[m.cursor]))
	case viewErrors:
		es := m.errorEntries()
		if m.cursor < 0 || m.cursor >= len(es) {
			return nil
		}
		e := es[m.cursor]
		m.detailTitle = "Error · " + e.at.Format("15:04:05")
		m.detailBody = e.title + "\n\n" + e.details
		m.show(viewDetail)
	}
	return nil
}
