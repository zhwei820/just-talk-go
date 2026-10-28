package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// AzureASRClient speaks the Azure Speech-to-Text WebSocket protocol (the one the
// official JS SDK uses), so no native Speech SDK library is needed.
type AzureASRClient struct {
	cfg       ASRConfig
	logger    *slog.Logger
	conn      *websocket.Conn
	connMu    sync.Mutex
	requestID string
	resultCh  chan ASRResult
	done      chan struct{}
	final     chan struct{}
	finalOnce sync.Once
	textMu    sync.RWMutex
	committed string
	partial   string
}

func NewAzureASRClient(cfg ASRConfig, logger *slog.Logger) *AzureASRClient {
	return &AzureASRClient{
		cfg: cfg, logger: logger,
		requestID: azureID(),
		resultCh:  make(chan ASRResult, 64),
		done:      make(chan struct{}),
		final:     make(chan struct{}),
	}
}

func azureID() string { return strings.ToUpper(strings.ReplaceAll(uuid.New().String(), "-", "")) }

func (c *AzureASRClient) Connect(ctx context.Context) error {
	if c.cfg.AzureKey == "" || c.cfg.AzureRegion == "" {
		return fmt.Errorf("azure_key 或 azure_region 未配置")
	}
	q := url.Values{}
	q.Set("language", c.cfg.Language)
	q.Set("format", "simple")
	q.Set("profanity", "raw")
	q.Set("X-ConnectionId", azureID())
	endpoint := fmt.Sprintf("wss://%s.stt.speech.microsoft.com/speech/recognition/conversation/cognitiveservices/v1?%s", c.cfg.AzureRegion, q.Encode())
	c.logger.Info("connecting to ASR", "provider", "azure", "region", c.cfg.AzureRegion)
	conn, resp, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPHeader: map[string][]string{"Ocp-Apim-Subscription-Key": {c.cfg.AzureKey}},
	})
	if err != nil {
		if resp != nil {
			return fmt.Errorf("websocket dial: HTTP %d %s: %w", resp.StatusCode, resp.Status, err)
		}
		return fmt.Errorf("websocket dial: %w", err)
	}
	c.conn = conn
	if err := c.sendInit(ctx); err != nil {
		conn.Close(websocket.StatusInternalError, "init failed")
		return fmt.Errorf("send init: %w", err)
	}
	c.logger.Info("ASR connected")
	return nil
}

func (c *AzureASRClient) sendInit(ctx context.Context) error {
	speechConfig := map[string]interface{}{
		"context": map[string]interface{}{
			"system": map[string]string{"version": "1.0.0"},
			"os":     map[string]string{"platform": "just-talk", "name": "just-talk", "version": "1"},
			"audio": map[string]interface{}{
				"source": map[string]interface{}{"bitspersample": 16, "channelcount": 1, "samplerate": 16000, "type": "Microphones"},
			},
		},
	}
	if err := c.writeText(ctx, "speech.config", false, speechConfig); err != nil {
		return err
	}
	if items := phraseItems(c.cfg.Hotwords); len(items) > 0 {
		speechContext := map[string]interface{}{
			"dgi": map[string]interface{}{
				"Groups": []map[string]interface{}{{"Type": "Generic", "Items": items}},
			},
		}
		if err := c.writeText(ctx, "speech.context", true, speechContext); err != nil {
			return err
		}
	}
	// The service expects the first audio chunk to carry a WAV header.
	return c.writeAudio(ctx, wavHeader(16000, 16, 1))
}

func phraseItems(words []string) []map[string]string {
	items := make([]map[string]string, 0, len(words))
	seen := make(map[string]struct{}, len(words))
	for _, word := range words {
		word = strings.TrimSpace(word)
		if word == "" {
			continue
		}
		if _, ok := seen[word]; ok {
			continue
		}
		seen[word] = struct{}{}
		items = append(items, map[string]string{"Text": word})
	}
	return items
}

func (c *AzureASRClient) writeText(ctx context.Context, path string, withRequestID bool, body interface{}) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Path: %s\r\n", path)
	if withRequestID {
		fmt.Fprintf(&b, "X-RequestId: %s\r\n", c.requestID)
	}
	fmt.Fprintf(&b, "X-Timestamp: %s\r\n", azureTimestamp())
	b.WriteString("Content-Type: application/json\r\n\r\n")
	b.Write(payload)
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.conn.Write(ctx, websocket.MessageText, []byte(b.String()))
}

// writeAudio sends one binary audio frame; an empty pcm marks end of audio.
func (c *AzureASRClient) writeAudio(ctx context.Context, pcm []byte) error {
	header := fmt.Sprintf("Path: audio\r\nX-RequestId: %s\r\nX-Timestamp: %s\r\nContent-Type: audio/x-wav\r\n", c.requestID, azureTimestamp())
	msg := make([]byte, 2, 2+len(header)+len(pcm))
	binary.BigEndian.PutUint16(msg, uint16(len(header)))
	msg = append(append(msg, header...), pcm...)
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.conn.Write(ctx, websocket.MessageBinary, msg)
}

func azureTimestamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func wavHeader(sampleRate, bits, channels int) []byte {
	var b bytes.Buffer
	blockAlign := channels * bits / 8
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(0))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint16(channels))
	binary.Write(&b, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&b, binary.LittleEndian, uint32(sampleRate*blockAlign))
	binary.Write(&b, binary.LittleEndian, uint16(blockAlign))
	binary.Write(&b, binary.LittleEndian, uint16(bits))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(0))
	return b.Bytes()
}

func (c *AzureASRClient) SendAudio(ctx context.Context, pcm []byte, isLast bool) error {
	if len(pcm) > 0 {
		if err := c.writeAudio(ctx, pcm); err != nil {
			return err
		}
	}
	if isLast {
		return c.writeAudio(ctx, nil)
	}
	return nil
}

func (c *AzureASRClient) Results() <-chan ASRResult { return c.resultCh }
func (c *AzureASRClient) Done() <-chan struct{}     { return c.done }
func (c *AzureASRClient) Final() <-chan struct{}    { return c.final }

func (c *AzureASRClient) LastText() string {
	c.textMu.RLock()
	defer c.textMu.RUnlock()
	return joinASRText(c.cfg.Language, c.committed, c.partial)
}

func (c *AzureASRClient) ReceiveLoop(ctx context.Context) {
	defer close(c.resultCh)
	defer close(c.done)
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			// The service rejects over-quota sessions with a close frame right
			// after the handshake; without surfacing it the session ends silently.
			var ce websocket.CloseError
			if errors.As(err, &ce) && ce.Code != websocket.StatusNormalClosure {
				reason, _, _ := strings.Cut(ce.Reason, " Cid:")
				c.logger.Warn("azure closed connection", "code", ce.Code, "reason", ce.Reason)
				c.resultCh <- ASRResult{Error: fmt.Errorf("Azure 断开连接: %s", strings.TrimSpace(reason))}
			}
			return
		}
		if typ == websocket.MessageText {
			c.handleMessage(data)
		}
	}
}

func (c *AzureASRClient) Close() error {
	if c.conn != nil {
		return c.conn.Close(websocket.StatusNormalClosure, "done")
	}
	return nil
}

func (c *AzureASRClient) handleMessage(data []byte) {
	head, body, _ := strings.Cut(string(data), "\r\n\r\n")
	var path string
	for _, line := range strings.Split(head, "\r\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), "path") {
			path = strings.ToLower(strings.TrimSpace(v))
		}
	}
	switch path {
	case "speech.hypothesis":
		var resp struct {
			Text string `json:"Text"`
		}
		if json.Unmarshal([]byte(body), &resp) != nil || resp.Text == "" {
			return
		}
		c.textMu.Lock()
		c.partial = resp.Text
		text := joinASRText(c.cfg.Language, c.committed, c.partial)
		c.textMu.Unlock()
		c.resultCh <- ASRResult{Text: text}
	case "speech.phrase":
		var resp struct {
			RecognitionStatus string `json:"RecognitionStatus"`
			DisplayText       string `json:"DisplayText"`
		}
		if json.Unmarshal([]byte(body), &resp) != nil {
			return
		}
		c.textMu.Lock()
		c.partial = ""
		if resp.RecognitionStatus == "Success" && resp.DisplayText != "" {
			c.committed = joinASRText(c.cfg.Language, c.committed, resp.DisplayText)
		}
		text := c.committed
		c.textMu.Unlock()
		switch resp.RecognitionStatus {
		case "Success", "NoMatch", "InitialSilenceTimeout", "BabbleTimeout", "EndOfDictation":
			if text != "" {
				c.resultCh <- ASRResult{Text: text}
			}
		default:
			c.resultCh <- ASRResult{Error: fmt.Errorf("azure recognition status: %s", resp.RecognitionStatus)}
		}
	case "turn.end":
		c.textMu.Lock()
		c.partial = ""
		text := c.committed
		c.textMu.Unlock()
		c.finalOnce.Do(func() {
			if text != "" {
				c.resultCh <- ASRResult{Text: text, IsFinal: true}
			}
			close(c.final)
		})
	}
}

// joinASRText joins recognized segments; CJK languages need no separator.
func joinASRText(language, a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	lang := strings.ToLower(language)
	if strings.HasPrefix(lang, "zh") || strings.HasPrefix(lang, "ja") || strings.HasPrefix(lang, "ko") {
		return a + b
	}
	return a + " " + b
}
