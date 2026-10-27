package ccds

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
)

var claudeAliases = []string{"sonnet", "opus", "haiku", "fable"}

// mapModel maps DeepSeek names through the config, passes Claude aliases and
// claude-* through, and sends anything else (client defaults like gpt-4o) to
// the deepseek-chat model.
func mapModel(name, chat, reasoner string) string {
	switch name {
	case "deepseek-chat":
		return chat
	case "deepseek-reasoner":
		return reasoner
	}
	if strings.HasPrefix(name, "claude-") || slices.Contains(claudeAliases, name) {
		return name
	}
	return chat
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// parseMessages returns an error for anything but an array of objects; an
// empty array is left for buildPrompt to reject.
func parseMessages(raw json.RawMessage) ([]message, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, newAPIError(400, "messages must be a non-empty array")
	}
	msgs := make([]message, len(items))
	for i, it := range items {
		if err := json.Unmarshal(it, &msgs[i]); err != nil {
			return nil, newAPIError(400, "messages must be an array of objects")
		}
	}
	return msgs, nil
}

// contentToText accepts a string or an array of {type: text|input_text|output_text, text}.
func contentToText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		var part struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(p, &part) != nil {
			continue
		}
		switch part.Type {
		case "text", "input_text", "output_text":
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

// buildPrompt flattens messages for `claude -p`, which takes a single prompt:
// system messages go to --system-prompt and multi-turn history becomes a transcript.
func buildPrompt(msgs []message) (system, prompt string, err error) {
	if len(msgs) == 0 {
		return "", "", newAPIError(400, "messages must be a non-empty array")
	}
	var systems []string
	var convo []message
	for _, m := range msgs {
		if m.Role == "system" {
			systems = append(systems, contentToText(m.Content))
		} else {
			convo = append(convo, m)
		}
	}
	system = strings.Join(systems, "\n\n")
	if len(convo) == 0 {
		return "", "", newAPIError(400, "messages must contain at least one non-system message")
	}
	if len(convo) == 1 && convo[0].Role == "user" {
		return system, contentToText(convo[0].Content), nil
	}

	names := map[string]string{"user": "User", "assistant": "Assistant", "tool": "Tool"}
	lines := []string{
		"Below is the conversation so far. Reply as the Assistant to the last User message.",
		"",
	}
	for _, m := range convo {
		name, ok := names[m.Role]
		if !ok {
			name = m.Role
		}
		lines = append(lines, "<"+name+">", contentToText(m.Content), "</"+name+">")
	}
	return system, strings.Join(lines, "\n"), nil
}

func claudeArgs(model, system string, stream bool) []string {
	format := "json"
	if stream {
		format = "stream-json"
	}
	args := []string{
		"-p",
		"--model", model,
		"--tools", "", // chat only: Claude Code must not touch local files or run commands
		"--no-session-persistence",
		"--output-format", format,
	}
	if stream {
		args = append(args, "--verbose", "--include-partial-messages")
	}
	if system != "" {
		args = append(args, "--system-prompt", system)
	}
	return args
}

var shellSafe = regexp.MustCompile(`^[\w@%+=:,./-]+$`)

func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		if shellSafe.MatchString(a) {
			quoted[i] = a
		} else {
			quoted[i] = "'" + strings.ReplaceAll(a, "'", `'"'"'`) + "'"
		}
	}
	return strings.Join(quoted, " ")
}

type claudeUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
}

type openAIUsage struct {
	PromptTokens          int `json:"prompt_tokens"`
	CompletionTokens      int `json:"completion_tokens"`
	TotalTokens           int `json:"total_tokens"`
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
}

func toUsage(u claudeUsage) openAIUsage {
	prompt := u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
	return openAIUsage{
		PromptTokens:          prompt,
		CompletionTokens:      u.OutputTokens,
		TotalTokens:           prompt + u.OutputTokens,
		PromptCacheHitTokens:  u.CacheReadTokens,
		PromptCacheMissTokens: prompt - u.CacheReadTokens,
	}
}

// resultText is the result string, or the whole event when there is none.
func resultText(result json.RawMessage, whole []byte) string {
	var s string
	if json.Unmarshal(result, &s) == nil && s != "" {
		return s
	}
	return strings.TrimSpace(string(whole))
}

type claudeRunner struct {
	bin     string
	timeout time.Duration
	// An empty directory, so claude does not pick up a CLAUDE.md or project settings.
	workdir string
}

func (c *claudeRunner) command(ctx context.Context, args []string, prompt string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Dir = c.workdir
	cmd.Stdin = strings.NewReader(prompt)
	return cmd
}

func (c *claudeRunner) runBlocking(ctx context.Context, args []string, prompt string) (string, claudeUsage, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cmd := c.command(ctx, args, prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", claudeUsage{}, &apiError{504, "claude timed out", "timeout"}
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return "", claudeUsage{}, &apiError{502, "claude failed: " + runErr.Error(), "api_error"}
	}
	var data struct {
		IsError bool            `json:"is_error"`
		Result  json.RawMessage `json:"result"`
		Usage   claudeUsage     `json:"usage"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &data); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		return "", claudeUsage{}, &apiError{502, fmt.Sprintf("claude failed (exit %d): %s", cmd.ProcessState.ExitCode(), detail), "api_error"}
	}
	if data.IsError {
		return "", claudeUsage{}, &apiError{502, resultText(data.Result, stdout.Bytes()), "api_error"}
	}
	var text string
	json.Unmarshal(data.Result, &text)
	return text, data.Usage, nil
}

type eventKind int

const (
	eventContent eventKind = iota
	eventReasoning
	eventDone
)

type streamEvent struct {
	kind   eventKind
	text   string
	usage  claudeUsage // eventDone only
	finish string      // eventDone only
}

type claudeStream struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	out    *bufio.Reader
	stderr bytes.Buffer
	usage  *claudeUsage
	finish string
	waited bool
	done   bool
}

func (c *claudeRunner) startStream(ctx context.Context, args []string, prompt string) (*claudeStream, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	s := &claudeStream{cmd: c.command(ctx, args, prompt), cancel: cancel, finish: "stop"}
	s.cmd.Stderr = &s.stderr
	out, err := s.cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, &apiError{502, "claude failed: " + err.Error(), "api_error"}
	}
	if err := s.cmd.Start(); err != nil {
		cancel()
		return nil, &apiError{502, "claude failed: " + err.Error(), "api_error"}
	}
	s.out = bufio.NewReader(out)
	return s, nil
}

// next returns content and reasoning deltas, then one eventDone, then io.EOF.
func (s *claudeStream) next() (streamEvent, error) {
	if s.done {
		return streamEvent{}, io.EOF
	}
	for {
		line, readErr := s.out.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			if ev, ok, err := s.parse(line); err != nil {
				return streamEvent{}, err
			} else if ok {
				return ev, nil
			}
		}
		if readErr != nil {
			break
		}
	}
	s.cmd.Wait()
	s.waited = true
	s.done = true
	if s.usage == nil {
		return streamEvent{}, &apiError{502, fmt.Sprintf("claude exited %d: %s", s.cmd.ProcessState.ExitCode(), strings.TrimSpace(s.stderr.String())), "api_error"}
	}
	return streamEvent{kind: eventDone, usage: *s.usage, finish: s.finish}, nil
}

func (s *claudeStream) parse(line []byte) (streamEvent, bool, error) {
	var ev struct {
		Type  string `json:"type"`
		Event struct {
			Type  string `json:"type"`
			Delta struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				Thinking   string `json:"thinking"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		} `json:"event"`
		IsError bool            `json:"is_error"`
		Result  json.RawMessage `json:"result"`
		Usage   *claudeUsage    `json:"usage"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return streamEvent{}, false, nil
	}
	switch ev.Type {
	case "stream_event":
		d := ev.Event.Delta
		switch ev.Event.Type {
		case "content_block_delta":
			if d.Type == "text_delta" && d.Text != "" {
				return streamEvent{kind: eventContent, text: d.Text}, true, nil
			}
			if d.Type == "thinking_delta" && d.Thinking != "" {
				return streamEvent{kind: eventReasoning, text: d.Thinking}, true, nil
			}
		case "message_delta":
			if d.StopReason == "max_tokens" {
				s.finish = "length"
			}
		}
	case "result":
		if ev.IsError {
			return streamEvent{}, false, &apiError{502, resultText(ev.Result, line), "api_error"}
		}
		s.usage = ev.Usage
	}
	return streamEvent{}, false, nil
}

func (s *claudeStream) close() {
	s.cancel()
	if !s.waited {
		s.cmd.Wait()
		s.waited = true
	}
}
