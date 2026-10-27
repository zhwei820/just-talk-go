package ccds

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMapModel(t *testing.T) {
	tests := map[string]string{
		"deepseek-chat":     "sonnet",
		"deepseek-reasoner": "opus",
		"haiku":             "haiku",
		"claude-opus-5-5":   "claude-opus-5-5",
		"gpt-4o":            "sonnet",
	}
	for in, want := range tests {
		if got := mapModel(in, "sonnet", "opus"); got != want {
			t.Errorf("mapModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildPrompt(t *testing.T) {
	parse := func(s string) []message {
		t.Helper()
		m, err := parseMessages(json.RawMessage(s))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	sys, prompt, err := buildPrompt(parse(`[{"role":"system","content":"a"},{"role":"system","content":[{"type":"text","text":"b"},{"type":"image_url"}]},{"role":"user","content":"hi"}]`))
	if err != nil || sys != "a\n\nb" || prompt != "hi" {
		t.Fatalf("single user: %q %q %v", sys, prompt, err)
	}

	_, prompt, _ = buildPrompt(parse(`[{"role":"user","content":"q"},{"role":"assistant","content":[{"type":"output_text","text":"a"}]},{"role":"user","content":"q2"}]`))
	want := "Below is the conversation so far. Reply as the Assistant to the last User message.\n\n<User>\nq\n</User>\n<Assistant>\na\n</Assistant>\n<User>\nq2\n</User>"
	if prompt != want {
		t.Fatalf("transcript:\n%s", prompt)
	}

	if _, _, err := buildPrompt(parse(`[{"role":"system","content":"a"}]`)); err == nil {
		t.Fatal("system only should fail")
	}
	for _, bad := range []string{`[]`, `null`, `"x"`, ``} {
		if m, err := parseMessages(json.RawMessage(bad)); err == nil {
			if _, _, err := buildPrompt(m); err == nil {
				t.Errorf("messages %q should fail", bad)
			}
		}
	}
}

func TestToUsage(t *testing.T) {
	got := toUsage(claudeUsage{InputTokens: 3, OutputTokens: 5, CacheReadTokens: 10, CacheCreationTokens: 2})
	want := openAIUsage{PromptTokens: 15, CompletionTokens: 5, TotalTokens: 20, PromptCacheHitTokens: 10, PromptCacheMissTokens: 5}
	if got != want {
		t.Fatalf("got %+v", got)
	}
}

// fakeClaude writes a script that ignores its arguments and prints out.
func fakeClaude(t *testing.T, out string) string {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "out")
	if err := os.WriteFile(data, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat >/dev/null\ncat '"+data+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

func newTestServer(t *testing.T, claudeOut, apiKey string) (*server, *httptest.Server) {
	t.Helper()
	v := &vault{dir: t.TempDir()}
	if err := v.loadToken(); err != nil {
		t.Fatal(err)
	}
	s := &server{
		cfg:    settings{apiKey: apiKey, chat: "sonnet", reasoner: "opus"},
		claude: &claudeRunner{bin: fakeClaude(t, claudeOut), timeout: 10 * time.Second, workdir: t.TempDir()},
		vault:  v,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts
}

func do(t *testing.T, method, url, body string, header map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	for k, v := range header {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestChatCompletions(t *testing.T) {
	_, ts := newTestServer(t, `{"type":"result","is_error":false,"result":"你好 <b>","usage":{"input_tokens":1,"output_tokens":2}}`, "k")
	body := `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`

	if code, _ := do(t, "POST", ts.URL+"/chat/completions", body, nil); code != 401 {
		t.Fatalf("no key: %d", code)
	}
	auth := map[string]string{"Authorization": "Bearer k"}
	code, out := do(t, "POST", ts.URL+"/v1/chat/completions/", body, auth)
	if code != 200 || !strings.Contains(out, `"content":"你好 <b>"`) || !strings.Contains(out, `"total_tokens":3`) {
		t.Fatalf("%d %s", code, out)
	}
	if code, _ := do(t, "POST", ts.URL+"/nope", body, auth); code != 404 {
		t.Fatalf("unknown path: %d", code)
	}
	if code, _ := do(t, "POST", ts.URL+"/chat/completions", "{", auth); code != 400 {
		t.Fatalf("bad json: %d", code)
	}

	code, out = do(t, "POST", ts.URL+"/responses", `{"input":"hi","instructions":"be brief"}`, auth)
	if code != 200 || !strings.Contains(out, `"output_text":"你好 <b>"`) || !strings.Contains(out, `"object":"response"`) {
		t.Fatalf("responses: %d %s", code, out)
	}
	if code, _ := do(t, "POST", ts.URL+"/responses", `{"input":"hi","stream":true}`, auth); code != 400 {
		t.Fatalf("responses stream: %d", code)
	}

	code, out = do(t, "GET", ts.URL+"/models", "", auth)
	if code != 200 || !strings.Contains(out, "deepseek-reasoner") {
		t.Fatalf("models: %d %s", code, out)
	}
}

func TestChatCompletionsClaudeError(t *testing.T) {
	_, ts := newTestServer(t, `{"type":"result","is_error":true,"result":"rate limited"}`, "")
	code, out := do(t, "POST", ts.URL+"/chat/completions", `{"messages":[{"role":"user","content":"hi"}]}`, nil)
	if code != 502 || !strings.Contains(out, "rate limited") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestStream(t *testing.T) {
	lines := strings.Join([]string{
		`{"type":"system"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"hmm"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hel"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}}`,
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"max_tokens"}}}`,
		`{"type":"result","is_error":false,"usage":{"input_tokens":4,"output_tokens":2}}`,
	}, "\n")
	_, ts := newTestServer(t, lines, "")
	code, out := do(t, "POST", ts.URL+"/chat/completions",
		`{"messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`, nil)
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	for _, want := range []string{
		`"delta":{"content":"","role":"assistant"}`,
		`"delta":{"reasoning_content":"hmm"}`,
		`"delta":{"content":"Hel"}`,
		`"finish_reason":"length"}],"usage":{"prompt_tokens":4`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("no [DONE]:\n%s", out)
	}
}

func TestStreamFailsBeforeFirstEvent(t *testing.T) {
	_, ts := newTestServer(t, `{"type":"result","is_error":true,"result":"boom"}`, "")
	code, out := do(t, "POST", ts.URL+"/chat/completions", `{"messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	if code != 502 || !strings.Contains(out, "boom") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestVault(t *testing.T) {
	s, ts := newTestServer(t, "", "")
	url := ts.URL + "/vault"
	auth := map[string]string{"Host": "127.0.0.1", "X-Vault-Token": s.vault.token}

	if code, _ := do(t, "GET", url, "", map[string]string{"Host": "evil.com", "X-Vault-Token": s.vault.token}); code != 403 {
		t.Fatalf("foreign host: %d", code)
	}
	if code, _ := do(t, "GET", url, "", map[string]string{"Host": "localhost:8888", "Origin": "https://evil.com", "X-Vault-Token": s.vault.token}); code != 403 {
		t.Fatalf("web origin: %d", code)
	}
	if code, _ := do(t, "GET", url, "", map[string]string{"Host": "localhost"}); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := do(t, "OPTIONS", url, "", auth); code < 400 {
		t.Fatalf("preflight must fail: %d", code)
	}
	if code, out := do(t, "GET", url, "", auth); code != 200 || out != `{"rev":0,"blob":null}` {
		t.Fatalf("empty vault: %d %s", code, out)
	}

	for _, bad := range []string{`{"blob":[],"baseRev":0}`, `{"blob":{},"baseRev":1.5}`, `{"blob":{}}`, `{"blob":{},"baseRev":null}`, ``} {
		if code, _ := do(t, "PUT", url, bad, auth); code != 400 {
			t.Errorf("PUT %s: %d", bad, code)
		}
	}

	for rev := range vaultBackupKeep + 3 {
		body := `{"blob":{"n":` + string(rune('0'+rev%10)) + `},"baseRev":` + itoa(rev) + `}`
		if code, out := do(t, "PUT", url, body, auth); code != 200 || out != `{"rev":`+itoa(rev+1)+`}` {
			t.Fatalf("PUT rev %d: %d %s", rev, code, out)
		}
	}
	code, out := do(t, "PUT", url, `{"blob":{},"baseRev":1}`, auth)
	if code != 409 || !strings.Contains(out, `"rev":33`) {
		t.Fatalf("conflict: %d %s", code, out)
	}

	backups, _ := os.ReadDir(s.vault.backupDir())
	if len(backups) != vaultBackupKeep || backups[0].Name() != "vault-000003.json" {
		t.Fatalf("backups: %d first %s", len(backups), backups[0].Name())
	}
	info, _ := os.Stat(s.vault.file())
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("vault mode %v", info.Mode())
	}
	if code, out := do(t, "GET", url, "", auth); code != 200 || !strings.Contains(out, `"rev":33`) || !strings.Contains(out, `"blob":{"n":2}`) {
		t.Fatalf("GET after PUT: %d %s", code, out)
	}

	token := s.vault.token
	again := &vault{dir: s.vault.dir}
	if err := again.loadToken(); err != nil || again.token != token {
		t.Fatalf("token not reused: %v", err)
	}
}

func itoa(n int) string { return strings.TrimSpace(string(marshal(n))) }
