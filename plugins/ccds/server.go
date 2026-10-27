package ccds

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

type apiError struct {
	status  int
	message string
	etype   string
}

func (e *apiError) Error() string { return e.message }

func newAPIError(status int, message string) *apiError {
	return &apiError{status, message, "invalid_request_error"}
}

type errorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Code    *string `json:"code"`
}

// marshal keeps non-ASCII text and <>& readable, like json.dumps(ensure_ascii=False).
func marshal(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body := marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	w.Write(body)
}

func writeError(w http.ResponseWriter, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		ae = &apiError{500, err.Error(), "api_error"}
	}
	writeJSON(w, ae.status, map[string]errorBody{"error": {ae.message, ae.etype, nil}})
}

type server struct {
	cfg    settings
	claude *claudeRunner
	vault  *vault
	logger *slog.Logger
}

type settings struct {
	apiKey         string
	chat, reasoner string
}

var vaultHosts = []string{"127.0.0.1", "localhost"}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() { s.logger.Info("ccds request", "method", r.Method, "path", r.URL.Path, "status", rec.status) }()

	path := strings.TrimRight(r.URL.Path, "/")
	var err error
	switch r.Method {
	case http.MethodGet:
		err = s.handleGet(rec, r, path)
	case http.MethodPut:
		err = s.handlePut(rec, r, path)
	case http.MethodPost:
		s.handlePost(rec, r, path)
	default:
		// Includes OPTIONS: a web page sending X-Vault-Token cross-origin
		// needs a CORS preflight, and that must fail.
		err = &apiError{501, "unsupported method", "invalid_request_error"}
	}
	if err != nil {
		writeError(rec, err)
	}
}

// checkVaultAuth rejects foreign Host headers (DNS rebinding) and any Origin
// except the browser extension.
func (s *server) checkVaultAuth(r *http.Request) error {
	host := r.Host
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	hostOK := slices.Contains(vaultHosts, host)
	origin := r.Header.Get("Origin")
	if !hostOK || (origin != "" && !strings.HasPrefix(origin, "chrome-extension://")) {
		return &apiError{403, "forbidden", "permission_error"}
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Vault-Token")), []byte(s.vault.token)) != 1 {
		return &apiError{401, "invalid vault token", "authentication_error"}
	}
	return nil
}

func (s *server) checkAuth(r *http.Request) error {
	if s.cfg.apiKey == "" {
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.cfg.apiKey)) != 1 {
		return &apiError{401, "Authentication Fails, invalid api key", "authentication_error"}
	}
	return nil
}

func (s *server) handlePut(w http.ResponseWriter, r *http.Request, path string) error {
	if path != "/vault" {
		return newAPIError(404, "not found")
	}
	if err := s.checkVaultAuth(r); err != nil {
		return err
	}
	if r.ContentLength > vaultMaxBytes {
		return newAPIError(413, "vault too large")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, vaultMaxBytes+1))
	if err != nil {
		return newAPIError(400, "read body: "+err.Error())
	}
	if len(raw) > vaultMaxBytes {
		return newAPIError(413, "vault too large")
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	var req struct {
		Blob    json.RawMessage `json:"blob"`
		BaseRev json.RawMessage `json:"baseRev"`
	}
	if json.Unmarshal(raw, &req) != nil {
		return newAPIError(400, "invalid JSON body")
	}
	var base int64
	blob := bytes.TrimSpace(req.Blob)
	if len(blob) == 0 || blob[0] != '{' || len(req.BaseRev) == 0 || string(req.BaseRev) == "null" ||
		json.Unmarshal(req.BaseRev, &base) != nil {
		return newAPIError(400, "blob (object) and baseRev (int) are required")
	}

	s.vault.mu.Lock()
	cur, err := s.vault.read()
	if err != nil {
		s.vault.mu.Unlock()
		return err
	}
	if base != cur.Rev {
		s.vault.mu.Unlock()
		writeJSON(w, 409, map[string]any{
			"error": map[string]string{"message": "vault changed on server", "type": "conflict"},
			"rev":   cur.Rev,
		})
		return nil
	}
	rec := vaultRecord{Rev: cur.Rev + 1, UpdatedAt: time.Now().Unix(), Blob: blob}
	err = s.vault.write(rec)
	s.vault.mu.Unlock()
	if err != nil {
		return err
	}
	s.logger.Info("vault saved", "rev", rec.Rev)
	writeJSON(w, 200, map[string]int64{"rev": rec.Rev})
	return nil
}

func (s *server) handleGet(w http.ResponseWriter, r *http.Request, path string) error {
	if path == "/vault" {
		if err := s.checkVaultAuth(r); err != nil {
			return err
		}
		s.vault.mu.Lock()
		rec, err := s.vault.read()
		s.vault.mu.Unlock()
		if err != nil {
			return err
		}
		writeJSON(w, 200, rec)
		return nil
	}
	if err := s.checkAuth(r); err != nil {
		return err
	}
	if path != "/models" && path != "/v1/models" {
		return newAPIError(404, "not found")
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	data := []model{
		{"deepseek-chat", "model", "cc-ds"},
		{"deepseek-reasoner", "model", "cc-ds"},
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
	return nil
}

type chatRequest struct {
	Model         string          `json:"model"`
	Messages      json.RawMessage `json:"messages"`
	Stream        bool            `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Input        json.RawMessage `json:"input"`
	Instructions json.RawMessage `json:"instructions"`
}

func (s *server) handlePost(w http.ResponseWriter, r *http.Request, path string) {
	cid := "chatcmpl-" + newHex()
	start := time.Now()
	elapsed := func() string { return fmt.Sprintf("%.1fs", time.Since(start).Seconds()) }
	err := func() error {
		// Read the body before auth and routing so 401 and 404 requests are logged too.
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return newAPIError(400, "read body: "+err.Error())
		}
		if len(raw) == 0 {
			raw = []byte("{}")
		}
		var req *chatRequest
		var pretty bytes.Buffer
		if json.Indent(&pretty, raw, "", "  ") == nil {
			s.logger.Debug("ccds request body", "id", cid, "path", r.URL.Path, "body", pretty.String())
			req = new(chatRequest)
			if json.Unmarshal(raw, req) != nil {
				req = nil
			}
		} else {
			s.logger.Debug("ccds request body", "id", cid, "path", r.URL.Path, "body", string(raw))
		}

		if err := s.checkAuth(r); err != nil {
			return err
		}
		var responses bool
		switch path {
		case "/chat/completions", "/v1/chat/completions":
		case "/responses", "/v1/responses":
			responses = true
		default:
			return newAPIError(404, "not found")
		}
		if req == nil {
			return newAPIError(400, "invalid JSON body")
		}
		if responses {
			return s.handleResponses(w, r, cid, start, req)
		}

		reqModel := orDefault(req.Model, "deepseek-chat")
		claudeModel := mapModel(reqModel, s.cfg.chat, s.cfg.reasoner)
		msgs, err := parseMessages(req.Messages)
		if err != nil {
			return err
		}
		system, prompt, err := buildPrompt(msgs)
		if err != nil {
			return err
		}
		args := claudeArgs(claudeModel, system, req.Stream)
		s.logger.Info("ccds model", "id", cid, "model", reqModel, "claude", claudeModel, "stream", req.Stream)
		s.logger.Debug("ccds command", "id", cid, "cmd", shellJoin(append([]string{s.claude.bin}, args...)))
		created := time.Now().Unix()

		if req.Stream {
			includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
			s.streamChat(w, r, cid, created, reqModel, args, prompt, includeUsage, start)
			return nil
		}

		text, usage, err := s.claude.runBlocking(r.Context(), args, prompt)
		if err != nil {
			return err
		}
		u := toUsage(usage)
		s.logger.Info("ccds response", "id", cid, "elapsed", elapsed(), "usage", u)
		s.logger.Debug("ccds response text", "id", cid, "text", text)
		type choice struct {
			Index        int               `json:"index"`
			Message      map[string]string `json:"message"`
			Logprobs     *struct{}         `json:"logprobs"`
			FinishReason string            `json:"finish_reason"`
		}
		writeJSON(w, 200, struct {
			ID      string      `json:"id"`
			Object  string      `json:"object"`
			Created int64       `json:"created"`
			Model   string      `json:"model"`
			Choices []choice    `json:"choices"`
			Usage   openAIUsage `json:"usage"`
		}{cid, "chat.completion", created, reqModel, []choice{{
			Message:      map[string]string{"role": "assistant", "content": text},
			FinishReason: "stop",
		}}, u})
		return nil
	}()
	if err != nil {
		var ae *apiError
		status := 500
		if errors.As(err, &ae) {
			status = ae.status
		}
		s.logger.Warn("ccds error", "id", cid, "status", status, "elapsed", elapsed(), "error", err)
		writeError(w, err)
	}
}

// handleResponses is the non-streaming subset of the OpenAI Responses API:
// input (string or message array) plus instructions.
func (s *server) handleResponses(w http.ResponseWriter, r *http.Request, cid string, start time.Time, req *chatRequest) error {
	if req.Stream {
		return newAPIError(400, "stream is not supported on /responses")
	}
	var msgs []message
	var input string
	if json.Unmarshal(req.Input, &input) == nil {
		msgs = []message{{Role: "user", Content: req.Input}}
	} else {
		var err error
		if msgs, err = parseMessages(req.Input); err != nil {
			return err
		}
	}
	if truthy(req.Instructions) {
		msgs = append([]message{{Role: "system", Content: req.Instructions}}, msgs...)
	}
	reqModel := orDefault(req.Model, "deepseek-chat")
	claudeModel := mapModel(reqModel, s.cfg.chat, s.cfg.reasoner)
	system, prompt, err := buildPrompt(msgs)
	if err != nil {
		return err
	}
	args := claudeArgs(claudeModel, system, false)
	s.logger.Info("ccds model", "id", cid, "model", reqModel, "claude", claudeModel, "stream", false)
	s.logger.Debug("ccds command", "id", cid, "cmd", shellJoin(append([]string{s.claude.bin}, args...)))

	text, usage, err := s.claude.runBlocking(r.Context(), args, prompt)
	if err != nil {
		return err
	}
	u := toUsage(usage)
	s.logger.Info("ccds response", "id", cid, "elapsed", fmt.Sprintf("%.1fs", time.Since(start).Seconds()), "usage", u)
	s.logger.Debug("ccds response text", "id", cid, "text", text)

	type content struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Annotations []any  `json:"annotations"`
	}
	type output struct {
		Type    string    `json:"type"`
		ID      string    `json:"id"`
		Status  string    `json:"status"`
		Role    string    `json:"role"`
		Content []content `json:"content"`
	}
	type usageOut struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	}
	writeJSON(w, 200, struct {
		ID         string   `json:"id"`
		Object     string   `json:"object"`
		CreatedAt  int64    `json:"created_at"`
		Status     string   `json:"status"`
		Model      string   `json:"model"`
		Output     []output `json:"output"`
		OutputText string   `json:"output_text"`
		Usage      usageOut `json:"usage"`
	}{
		ID:        "resp_" + strings.TrimPrefix(cid, "chatcmpl-"),
		Object:    "response",
		CreatedAt: start.Unix(),
		Status:    "completed",
		Model:     reqModel,
		Output: []output{{
			Type: "message", ID: "msg_" + newHex(), Status: "completed", Role: "assistant",
			Content: []content{{Type: "output_text", Text: text, Annotations: []any{}}},
		}},
		OutputText: text,
		Usage:      usageOut{u.PromptTokens, u.CompletionTokens, u.TotalTokens},
	})
	return nil
}

func (s *server) streamChat(w http.ResponseWriter, r *http.Request, cid string, created int64, reqModel string, args []string, prompt string, includeUsage bool, start time.Time) {
	elapsed := func() string { return fmt.Sprintf("%.1fs", time.Since(start).Seconds()) }
	fail := func(err error) {
		s.logger.Warn("ccds error", "id", cid, "elapsed", elapsed(), "error", err)
		writeError(w, err)
	}
	stream, err := s.claude.startStream(r.Context(), args, prompt)
	if err != nil {
		fail(err)
		return
	}
	defer stream.close()
	// Wait for the first event before sending 200, so startup failures still get a real status code.
	first, err := stream.next()
	if err != nil {
		fail(err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "close")
	w.WriteHeader(200)
	rc := http.NewResponseController(w)

	send := func(v any) error {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", marshal(v)); err != nil {
			return err
		}
		return rc.Flush()
	}
	type choice struct {
		Index        int               `json:"index"`
		Delta        map[string]string `json:"delta"`
		Logprobs     *struct{}         `json:"logprobs"`
		FinishReason *string           `json:"finish_reason"`
	}
	type chunkBody struct {
		ID      string       `json:"id"`
		Object  string       `json:"object"`
		Created int64        `json:"created"`
		Model   string       `json:"model"`
		Choices []choice     `json:"choices"`
		Usage   *openAIUsage `json:"usage,omitempty"`
	}
	chunk := func(delta map[string]string, finish *string, usage *openAIUsage) error {
		return send(chunkBody{cid, "chat.completion.chunk", created, reqModel,
			[]choice{{Delta: delta, FinishReason: finish}}, usage})
	}

	var content, reasoning strings.Builder
	emit := func(ev streamEvent) error {
		switch ev.kind {
		case eventContent:
			content.WriteString(ev.text)
			return chunk(map[string]string{"content": ev.text}, nil, nil)
		case eventReasoning:
			reasoning.WriteString(ev.text)
			return chunk(map[string]string{"reasoning_content": ev.text}, nil, nil)
		default:
			u := toUsage(ev.usage)
			s.logger.Info("ccds response", "id", cid, "elapsed", elapsed(), "finish", ev.finish, "usage", u)
			var usage *openAIUsage
			if includeUsage {
				usage = &u
			}
			return chunk(map[string]string{}, &ev.finish, usage)
		}
	}
	logOutput := func(status string) {
		text := content.String()
		if reasoning.Len() > 0 {
			text = "<reasoning>\n" + reasoning.String() + "\n</reasoning>\n" + text
		}
		s.logger.Info("ccds stream "+status, "id", cid, "elapsed", elapsed())
		s.logger.Debug("ccds stream text", "id", cid, "text", text)
	}

	err = chunk(map[string]string{"role": "assistant", "content": ""}, nil, nil)
	if err == nil {
		err = emit(first)
	}
	for err == nil {
		var ev streamEvent
		if ev, err = stream.next(); err == nil {
			err = emit(ev)
		}
	}
	var ae *apiError
	switch {
	case errors.Is(err, io.EOF):
		logOutput("done")
	case r.Context().Err() != nil || !errors.As(err, &ae):
		logOutput("client disconnected")
		return
	default:
		logOutput(fmt.Sprintf("error %d: %s", ae.status, ae.message))
		if send(map[string]errorBody{"error": {ae.message, ae.etype, nil}}) != nil {
			return
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	rc.Flush()
}

func newHex() string { return strings.ReplaceAll(uuid.NewString(), "-", "") }

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// truthy mirrors Python truthiness for a JSON value.
func truthy(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", "false", "0", `""`, "[]", "{}":
		return false
	}
	return true
}
