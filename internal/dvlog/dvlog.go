// Package dvlog is the debug-verbose trace (-dv / -dvlog): timestamped
// round/tool/size log for diagnosing slow runs. Off by default; every helper
// is a no-op until Init or Enable turns it on, so the hot loop pays nothing
// normally.
package dvlog

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sashabaranov/go-openai"
)

var (
	verbose bool
	out     io.Writer = os.Stderr
	logFile *os.File
	start   time.Time
	mu      sync.Mutex
)

// Init enables the trace. logPath (from -dvlog) implies verbose and sends
// the log to a file instead of stderr — use it in TUI mode, where stderr
// writes would corrupt the fullscreen view.
func Init(enabled bool, logPath string) error {
	if logPath != "" {
		enabled = true
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("dvlog: %w", err)
		}
		if logFile != nil {
			_ = logFile.Close()
		}
		logFile = f
		out = f
	} else {
		out = os.Stderr
	}
	verbose = enabled
	if enabled {
		start = time.Now()
	}
	return nil
}

// Close releases the log file, if any.
func Close() {
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
}

// Enabled reports whether the trace is on.
func Enabled() bool { return verbose }

// Enable switches the trace on or off (tests and runtime toggles).
func Enable(on bool) {
	verbose = on
	if on {
		start = time.Now()
	}
}

// SetOutput redirects the trace (tests) and returns the previous writer.
func SetOutput(w io.Writer) io.Writer {
	prev := out
	out = w
	return prev
}

// Logf appends one timestamped line: wall clock + elapsed since trace start.
func Logf(format string, args ...any) {
	if !verbose {
		return
	}
	now := time.Now()
	el := now.Sub(start)
	if el < 0 {
		el = 0
	}
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprintf(out, "[dv %s +%s] %s\n", now.Format("15:04:05.000"), Dur(el), fmt.Sprintf(format, args...))
}

// Dur formats a duration compactly for the trace.
func Dur(d time.Duration) string {
	if d >= time.Minute {
		m := int(d.Minutes())
		return fmt.Sprintf("%dm%04.1fs", m, d.Seconds()-float64(m*60))
	}
	return fmt.Sprintf("%.3fs", d.Seconds())
}

// Preview flattens s to one line and caps it at n runes for log previews.
func Preview(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// SendBytes returns the JSON wire size of a request struct; ok=false when
// it cannot marshal (never blocks the send — the request is unaffected).
func SendBytes(v any) (int, bool) {
	raw, err := json.Marshal(v)
	if err != nil {
		return 0, false
	}
	return len(raw), true
}

// HistoryLine summarizes a history: message counts per role, total content
// chars, and the fail-fast token estimate (computed by the caller, which
// owns the estimator).
func HistoryLine(history []openai.ChatCompletionMessage, estTokens int) string {
	var sys, user, asst, tool int
	var chars int
	for _, m := range history {
		chars += len([]rune(m.Content))
		for _, tc := range m.ToolCalls {
			chars += len([]rune(tc.Function.Arguments))
		}
		switch m.Role {
		case openai.ChatMessageRoleSystem:
			sys++
		case openai.ChatMessageRoleUser:
			user++
		case openai.ChatMessageRoleAssistant:
			asst++
		case openai.ChatMessageRoleTool:
			tool++
		}
	}
	return fmt.Sprintf("msgs=%d(sys=%d,user=%d,asst=%d,tool=%d) chars=%d est~%dtok",
		len(history), sys, user, asst, tool, chars, estTokens)
}

// CallsLine summarizes one round's tool calls: name + arg bytes each.
func CallsLine(calls []openai.ToolCall) string {
	parts := make([]string, 0, len(calls))
	for _, tc := range calls {
		parts = append(parts, fmt.Sprintf("%s(args=%dB)", tc.Function.Name, len(tc.Function.Arguments)))
	}
	return strings.Join(parts, " ")
}

// TailDetail lists the last n messages with sizes and short previews, so
// the log shows what was actually sent without dumping full file contents.
func TailDetail(history []openai.ChatCompletionMessage, n int) string {
	if len(history) == 0 {
		return "  (empty history)"
	}
	names := map[string]string{}
	for _, m := range history {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				names[tc.ID] = tc.Function.Name
			}
		}
	}
	start := len(history) - n
	if start < 0 {
		start = 0
	}
	var b strings.Builder
	for i := start; i < len(history); i++ {
		m := history[i]
		fmt.Fprintf(&b, "  [%d] %s content=%dB", i, m.Role, len(m.Content))
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, " tc=%s args=%dB %q", tc.Function.Name, len(tc.Function.Arguments), Preview(tc.Function.Arguments, 160))
		}
		if m.Role == openai.ChatMessageRoleTool {
			name := names[m.ToolCallID]
			if name == "" {
				name = "?"
			}
			fmt.Fprintf(&b, " ->%s %q", name, Preview(m.Content, 160))
		} else if strings.TrimSpace(m.Content) != "" {
			fmt.Fprintf(&b, " %q", Preview(m.Content, 160))
		}
		if i+1 < len(history) {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
