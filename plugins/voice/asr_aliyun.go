package voice

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// AliyunASRClient speaks the Alibaba Cloud NLS real-time transcription
// WebSocket protocol (https://help.aliyun.com/document_detail/324262.html).
// Language is decided by the appkey's project settings, not by ASRConfig.
type AliyunASRClient struct {
	cfg       ASRConfig
	logger    *slog.Logger
	conn      *websocket.Conn
	connMu    sync.Mutex
	taskID    string
	resultCh  chan ASRResult
	done      chan struct{}
	final     chan struct{}
	finalOnce sync.Once
	textMu    sync.RWMutex
	committed string
	partial   string
}

func NewAliyunASRClient(cfg ASRConfig, logger *slog.Logger) *AliyunASRClient {
	return &AliyunASRClient{
		cfg: cfg, logger: logger,
		taskID:   aliyunID(),
		resultCh: make(chan ASRResult, 64),
		done:     make(chan struct{}),
		final:    make(chan struct{}),
	}
}

func aliyunID() string { return strings.ReplaceAll(uuid.New().String(), "-", "") }

type aliyunMessage struct {
	Header struct {
		Name       string `json:"name"`
		Status     int    `json:"status"`
		StatusText string `json:"status_text"`
	} `json:"header"`
	Payload struct {
		Result string `json:"result"`
	} `json:"payload"`
}

func (c *AliyunASRClient) Connect(ctx context.Context) error {
	if c.cfg.AliyunAppKey == "" || c.cfg.AliyunAccessKeyID == "" || c.cfg.AliyunAccessKeySecret == "" {
		return fmt.Errorf("aliyun_appkey、aliyun_access_key_id 或 aliyun_access_key_secret 未配置")
	}
	token, err := aliyunToken(ctx, c.cfg.AliyunAccessKeyID, c.cfg.AliyunAccessKeySecret)
	if err != nil {
		return fmt.Errorf("获取阿里云 token: %w", err)
	}
	c.logger.Info("connecting to ASR", "provider", "aliyun")
	conn, resp, err := websocket.Dial(ctx, "wss://nls-gateway-cn-shanghai.aliyuncs.com/ws/v1", &websocket.DialOptions{
		HTTPHeader: map[string][]string{"X-NLS-Token": {token}},
	})
	if err != nil {
		if resp != nil {
			return fmt.Errorf("websocket dial: HTTP %d %s: %w", resp.StatusCode, resp.Status, err)
		}
		return fmt.Errorf("websocket dial: %w", err)
	}
	c.conn = conn
	start := map[string]any{
		"format":                            "pcm",
		"sample_rate":                       16000,
		"enable_intermediate_result":        true,
		"enable_punctuation_prediction":     true,
		"enable_inverse_text_normalization": true,
	}
	if err := c.writeCommand(ctx, "StartTranscription", start); err != nil {
		conn.Close(websocket.StatusInternalError, "start failed")
		return fmt.Errorf("send start: %w", err)
	}
	// Audio sent before TranscriptionStarted is rejected.
	_, data, err := conn.Read(ctx)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "start failed")
		return fmt.Errorf("read start: %w", err)
	}
	var msg aliyunMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		conn.Close(websocket.StatusInternalError, "bad start")
		return fmt.Errorf("parse start: %w", err)
	}
	if msg.Header.Name != "TranscriptionStarted" {
		conn.Close(websocket.StatusNormalClosure, "")
		return fmt.Errorf("阿里云 %d: %s", msg.Header.Status, msg.Header.StatusText)
	}
	c.logger.Info("ASR connected")
	return nil
}

func (c *AliyunASRClient) writeCommand(ctx context.Context, name string, payload map[string]any) error {
	msg := map[string]any{
		"header": map[string]string{
			"message_id": aliyunID(),
			"task_id":    c.taskID,
			"namespace":  "SpeechTranscriber",
			"name":       name,
			"appkey":     c.cfg.AliyunAppKey,
		},
	}
	if payload != nil {
		msg["payload"] = payload
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.conn.Write(ctx, websocket.MessageText, data)
}

func (c *AliyunASRClient) SendAudio(ctx context.Context, pcm []byte, isLast bool) error {
	if len(pcm) > 0 {
		c.connMu.Lock()
		err := c.conn.Write(ctx, websocket.MessageBinary, pcm)
		c.connMu.Unlock()
		if err != nil {
			return err
		}
	}
	if isLast {
		return c.writeCommand(ctx, "StopTranscription", nil)
	}
	return nil
}

func (c *AliyunASRClient) Results() <-chan ASRResult { return c.resultCh }
func (c *AliyunASRClient) Done() <-chan struct{}     { return c.done }
func (c *AliyunASRClient) Final() <-chan struct{}    { return c.final }

func (c *AliyunASRClient) LastText() string {
	c.textMu.RLock()
	defer c.textMu.RUnlock()
	return joinASRText(c.cfg.Language, c.committed, c.partial)
}

func (c *AliyunASRClient) ReceiveLoop(ctx context.Context) {
	defer close(c.resultCh)
	defer close(c.done)
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			if errors.As(err, &ce) && ce.Code != websocket.StatusNormalClosure {
				c.logger.Warn("aliyun closed connection", "code", ce.Code, "reason", ce.Reason)
				c.resultCh <- ASRResult{Error: fmt.Errorf("阿里云断开连接: %s", ce.Reason)}
			}
			return
		}
		c.handleMessage(data)
	}
}

func (c *AliyunASRClient) Close() error {
	if c.conn != nil {
		return c.conn.Close(websocket.StatusNormalClosure, "done")
	}
	return nil
}

func (c *AliyunASRClient) handleMessage(data []byte) {
	var msg aliyunMessage
	if json.Unmarshal(data, &msg) != nil {
		return
	}
	c.textMu.Lock()
	switch msg.Header.Name {
	case "TranscriptionResultChanged":
		c.partial = msg.Payload.Result
	case "SentenceEnd":
		c.committed = joinASRText(c.cfg.Language, c.committed, msg.Payload.Result)
		c.partial = ""
	}
	text := joinASRText(c.cfg.Language, c.committed, c.partial)
	c.textMu.Unlock()

	switch msg.Header.Name {
	case "TranscriptionResultChanged", "SentenceEnd":
		if text != "" {
			c.resultCh <- ASRResult{Text: text}
		}
	case "TranscriptionCompleted":
		c.finalOnce.Do(func() {
			if text != "" {
				c.resultCh <- ASRResult{Text: text, IsFinal: true}
			}
			close(c.final)
		})
	case "TaskFailed":
		c.resultCh <- ASRResult{Error: fmt.Errorf("阿里云 %d: %s", msg.Header.Status, msg.Header.StatusText)}
	}
}

// Tokens are valid for hours; caching avoids an extra HTTPS round trip
// before every recording.
var aliyunTokenCache struct {
	sync.Mutex
	key     string
	token   string
	expires time.Time
}

func aliyunToken(ctx context.Context, keyID, secret string) (string, error) {
	aliyunTokenCache.Lock()
	defer aliyunTokenCache.Unlock()
	if aliyunTokenCache.key == keyID && time.Until(aliyunTokenCache.expires) > 5*time.Minute {
		return aliyunTokenCache.token, nil
	}
	params := map[string]string{
		"AccessKeyId":      keyID,
		"Action":           "CreateToken",
		"Format":           "JSON",
		"RegionId":         "cn-shanghai",
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   uuid.New().String(),
		"SignatureVersion": "1.0",
		"Timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"Version":          "2019-02-28",
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, len(keys))
	for i, k := range keys {
		pairs[i] = popEscape(k) + "=" + popEscape(params[k])
	}
	query := strings.Join(pairs, "&")
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte("GET&" + popEscape("/") + "&" + popEscape(query)))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		"https://nls-meta.cn-shanghai.aliyuncs.com/?Signature="+popEscape(signature)+"&"+query, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var out struct {
		Message string `json:"Message"`
		Token   struct {
			ID         string `json:"Id"`
			ExpireTime int64  `json:"ExpireTime"`
		} `json:"Token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out.Token.ID == "" {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, out.Message)
	}
	aliyunTokenCache.key = keyID
	aliyunTokenCache.token = out.Token.ID
	aliyunTokenCache.expires = time.Unix(out.Token.ExpireTime, 0)
	return out.Token.ID, nil
}

// popEscape is the percent-encoding Alibaba Cloud's RPC signature requires.
func popEscape(s string) string {
	s = url.QueryEscape(s)
	s = strings.ReplaceAll(s, "+", "%20")
	s = strings.ReplaceAll(s, "*", "%2A")
	return strings.ReplaceAll(s, "%7E", "~")
}
