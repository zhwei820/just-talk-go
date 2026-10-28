package voice

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// TencentASRClient speaks the Tencent Cloud real-time ASR WebSocket protocol
// (https://cloud.tencent.com/document/product/1093/48982).
type TencentASRClient struct {
	cfg       ASRConfig
	logger    *slog.Logger
	conn      *websocket.Conn
	connMu    sync.Mutex
	resultCh  chan ASRResult
	done      chan struct{}
	final     chan struct{}
	finalOnce sync.Once
	textMu    sync.RWMutex
	committed string
	partial   string
}

func NewTencentASRClient(cfg ASRConfig, logger *slog.Logger) *TencentASRClient {
	return &TencentASRClient{
		cfg: cfg, logger: logger,
		resultCh: make(chan ASRResult, 64),
		done:     make(chan struct{}),
		final:    make(chan struct{}),
	}
}

type tencentResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Final   int    `json:"final"`
	Result  struct {
		SliceType    int    `json:"slice_type"`
		VoiceTextStr string `json:"voice_text_str"`
	} `json:"result"`
}

func (c *TencentASRClient) Connect(ctx context.Context) error {
	if c.cfg.TencentAppID == "" || c.cfg.TencentSecretID == "" || c.cfg.TencentSecretKey == "" {
		return fmt.Errorf("tencent_app_id、tencent_secret_id 或 tencent_secret_key 未配置")
	}
	now := time.Now().Unix()
	params := map[string]string{
		"secretid":          c.cfg.TencentSecretID,
		"timestamp":         strconv.FormatInt(now, 10),
		"expired":           strconv.FormatInt(now+3600, 10),
		"nonce":             strconv.Itoa(rand.IntN(1_000_000_000)),
		"engine_model_type": tencentEngine(c.cfg.Language),
		"voice_id":          uuid.New().String(),
		"voice_format":      "1",
		"needvad":           "1",
	}
	if hw := tencentHotwords(c.cfg.Hotwords); hw != "" {
		params["hotword_list"] = hw
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var raw, enc strings.Builder
	for i, k := range keys {
		if i > 0 {
			raw.WriteByte('&')
			enc.WriteByte('&')
		}
		fmt.Fprintf(&raw, "%s=%s", k, params[k])
		fmt.Fprintf(&enc, "%s=%s", k, url.QueryEscape(params[k]))
	}
	// The signature covers the unescaped query; the request carries it escaped.
	path := "asr.cloud.tencent.com/asr/v2/" + c.cfg.TencentAppID
	mac := hmac.New(sha1.New, []byte(c.cfg.TencentSecretKey))
	mac.Write([]byte(path + "?" + raw.String()))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	endpoint := "wss://" + path + "?" + enc.String() + "&signature=" + url.QueryEscape(signature)

	c.logger.Info("connecting to ASR", "provider", "tencent")
	conn, resp, err := websocket.Dial(ctx, endpoint, nil)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("websocket dial: HTTP %d %s: %w", resp.StatusCode, resp.Status, err)
		}
		return fmt.Errorf("websocket dial: %w", err)
	}
	// The server acknowledges the handshake (or reports auth errors) first.
	_, data, err := conn.Read(ctx)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "handshake failed")
		return fmt.Errorf("read handshake: %w", err)
	}
	var ack tencentResponse
	if err := json.Unmarshal(data, &ack); err != nil {
		conn.Close(websocket.StatusInternalError, "bad handshake")
		return fmt.Errorf("parse handshake: %w", err)
	}
	if ack.Code != 0 {
		conn.Close(websocket.StatusNormalClosure, "")
		return fmt.Errorf("腾讯云 %d: %s", ack.Code, ack.Message)
	}
	c.conn = conn
	c.logger.Info("ASR connected")
	return nil
}

func tencentEngine(language string) string {
	lang := strings.ToLower(language)
	for _, prefix := range []string{"en", "ja", "ko"} {
		if strings.HasPrefix(lang, prefix) {
			return "16k_" + prefix
		}
	}
	return "16k_zh"
}

// tencentHotwords builds the temporary hotword_list ("词|权重,词|权重").
func tencentHotwords(words []string) string {
	var items []string
	seen := make(map[string]struct{}, len(words))
	for _, word := range words {
		word = strings.TrimSpace(word)
		if word == "" || strings.ContainsAny(word, "|,") {
			continue
		}
		if _, ok := seen[word]; ok {
			continue
		}
		seen[word] = struct{}{}
		items = append(items, word+"|10")
	}
	return strings.Join(items, ",")
}

func (c *TencentASRClient) SendAudio(ctx context.Context, pcm []byte, isLast bool) error {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if len(pcm) > 0 {
		if err := c.conn.Write(ctx, websocket.MessageBinary, pcm); err != nil {
			return err
		}
	}
	if isLast {
		return c.conn.Write(ctx, websocket.MessageText, []byte(`{"type":"end"}`))
	}
	return nil
}

func (c *TencentASRClient) Results() <-chan ASRResult { return c.resultCh }
func (c *TencentASRClient) Done() <-chan struct{}     { return c.done }
func (c *TencentASRClient) Final() <-chan struct{}    { return c.final }

func (c *TencentASRClient) LastText() string {
	c.textMu.RLock()
	defer c.textMu.RUnlock()
	return joinASRText(c.cfg.Language, c.committed, c.partial)
}

func (c *TencentASRClient) ReceiveLoop(ctx context.Context) {
	defer close(c.resultCh)
	defer close(c.done)
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			if errors.As(err, &ce) && ce.Code != websocket.StatusNormalClosure {
				c.logger.Warn("tencent closed connection", "code", ce.Code, "reason", ce.Reason)
				c.resultCh <- ASRResult{Error: fmt.Errorf("腾讯云断开连接: %s", ce.Reason)}
			}
			return
		}
		c.handleMessage(data)
	}
}

func (c *TencentASRClient) Close() error {
	if c.conn != nil {
		return c.conn.Close(websocket.StatusNormalClosure, "done")
	}
	return nil
}

func (c *TencentASRClient) handleMessage(data []byte) {
	var resp tencentResponse
	if json.Unmarshal(data, &resp) != nil {
		return
	}
	if resp.Code != 0 {
		c.resultCh <- ASRResult{Error: fmt.Errorf("腾讯云 %d: %s", resp.Code, resp.Message)}
		return
	}
	c.textMu.Lock()
	if resp.Result.SliceType == 2 {
		c.committed = joinASRText(c.cfg.Language, c.committed, resp.Result.VoiceTextStr)
		c.partial = ""
	} else if resp.Result.VoiceTextStr != "" {
		c.partial = resp.Result.VoiceTextStr
	}
	text := joinASRText(c.cfg.Language, c.committed, c.partial)
	c.textMu.Unlock()
	if resp.Final == 1 {
		c.finalOnce.Do(func() {
			if text != "" {
				c.resultCh <- ASRResult{Text: text, IsFinal: true}
			}
			close(c.final)
		})
		return
	}
	if text != "" {
		c.resultCh <- ASRResult{Text: text}
	}
}
