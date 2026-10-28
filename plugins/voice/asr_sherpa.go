package voice

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

// SherpaASRClient recognizes locally with a SenseVoice model via sherpa-onnx.
// SenseVoice is non-streaming: audio is buffered and decoded once when the
// final chunk arrives, so there are no partial results.
type SherpaASRClient struct {
	cfg       ASRConfig
	logger    *slog.Logger
	mu        sync.Mutex
	samples   []float32
	text      string
	resultCh  chan ASRResult
	done      chan struct{}
	final     chan struct{}
	finalOnce sync.Once
	closed    chan struct{}
	closeOnce sync.Once
}

func NewSherpaASRClient(cfg ASRConfig, logger *slog.Logger) *SherpaASRClient {
	return &SherpaASRClient{
		cfg: cfg, logger: logger,
		resultCh: make(chan ASRResult, 1),
		done:     make(chan struct{}),
		final:    make(chan struct{}),
		closed:   make(chan struct{}),
	}
}

// The recognizer holds a few hundred MB of model weights, so it is loaded once
// and shared. The mutex also serializes Decode across overlapping sessions.
var sherpaShared struct {
	sync.Mutex
	dir        string
	recognizer *sherpa.OfflineRecognizer
}

func sherpaRecognizerLocked(dir string, logger *slog.Logger) (*sherpa.OfflineRecognizer, error) {
	if sherpaShared.recognizer != nil && sherpaShared.dir == dir {
		return sherpaShared.recognizer, nil
	}
	model := filepath.Join(dir, "model.int8.onnx")
	if _, err := os.Stat(model); err != nil {
		model = filepath.Join(dir, "model.onnx")
	}
	tokens := filepath.Join(dir, "tokens.txt")
	for _, f := range []string{model, tokens} {
		if _, err := os.Stat(f); err != nil {
			return nil, fmt.Errorf("本地模型文件缺失: %s", f)
		}
	}
	start := time.Now()
	cfg := sherpa.OfflineRecognizerConfig{}
	cfg.FeatConfig.SampleRate = 16000
	cfg.FeatConfig.FeatureDim = 80
	cfg.ModelConfig.SenseVoice.Model = model
	cfg.ModelConfig.SenseVoice.Language = "auto"
	cfg.ModelConfig.SenseVoice.UseInverseTextNormalization = 1
	cfg.ModelConfig.Tokens = tokens
	cfg.ModelConfig.NumThreads = 4
	cfg.ModelConfig.Provider = "cpu"
	cfg.DecodingMethod = "greedy_search"
	r := sherpa.NewOfflineRecognizer(&cfg)
	if r == nil {
		return nil, fmt.Errorf("加载本地模型失败: %s", dir)
	}
	if sherpaShared.recognizer != nil {
		sherpa.DeleteOfflineRecognizer(sherpaShared.recognizer)
	}
	sherpaShared.dir, sherpaShared.recognizer = dir, r
	logger.Info("sherpa model loaded", "dir", dir, "elapsed", time.Since(start).Round(time.Millisecond))
	return r, nil
}

// PreloadSherpa loads the model ahead of the first recording.
func PreloadSherpa(dir string, logger *slog.Logger) error {
	sherpaShared.Lock()
	defer sherpaShared.Unlock()
	_, err := sherpaRecognizerLocked(dir, logger)
	return err
}

func (c *SherpaASRClient) Connect(ctx context.Context) error {
	c.logger.Info("connecting to ASR", "provider", "sherpa")
	if err := PreloadSherpa(c.cfg.SherpaModelDir, c.logger); err != nil {
		return err
	}
	c.logger.Info("ASR connected")
	return nil
}

func (c *SherpaASRClient) SendAudio(ctx context.Context, pcm []byte, isLast bool) error {
	c.mu.Lock()
	for i := 0; i+1 < len(pcm); i += 2 {
		c.samples = append(c.samples, float32(int16(binary.LittleEndian.Uint16(pcm[i:])))/32768)
	}
	c.mu.Unlock()
	if isLast {
		go c.finalOnce.Do(c.decode)
	}
	return nil
}

func (c *SherpaASRClient) decode() {
	defer close(c.final)
	c.mu.Lock()
	samples := c.samples
	c.samples = nil
	c.mu.Unlock()
	// Shorter than 0.1s carries no speech; the C API also rejects empty input.
	if len(samples) < 1600 {
		return
	}
	start := time.Now()
	sherpaShared.Lock()
	r, err := sherpaRecognizerLocked(c.cfg.SherpaModelDir, c.logger)
	var text string
	if err == nil {
		stream := sherpa.NewOfflineStream(r)
		stream.AcceptWaveform(16000, samples)
		r.Decode(stream)
		text = stream.GetResult().Text
		sherpa.DeleteOfflineStream(stream)
	}
	sherpaShared.Unlock()
	if err != nil {
		c.logger.Warn("sherpa decode failed", "error", err)
		return
	}
	c.logger.Debug("sherpa decoded", "audio", time.Duration(len(samples))*time.Second/16000, "elapsed", time.Since(start).Round(time.Millisecond))
	c.mu.Lock()
	c.text = text
	c.mu.Unlock()
}

func (c *SherpaASRClient) Results() <-chan ASRResult { return c.resultCh }
func (c *SherpaASRClient) Done() <-chan struct{}     { return c.done }
func (c *SherpaASRClient) Final() <-chan struct{}    { return c.final }

func (c *SherpaASRClient) LastText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text
}

// ReceiveLoop owns resultCh so the decode goroutine never sends on a closed channel.
func (c *SherpaASRClient) ReceiveLoop(ctx context.Context) {
	defer close(c.resultCh)
	defer close(c.done)
	select {
	case <-c.final:
		if text := c.LastText(); text != "" {
			c.resultCh <- ASRResult{Text: text, IsFinal: true}
		}
	case <-c.closed:
	case <-ctx.Done():
	}
}

func (c *SherpaASRClient) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}
