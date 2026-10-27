// Package ccds serves a DeepSeek / OpenAI compatible Chat Completions API
// backed by the local Claude Code CLI (`claude -p`), plus a ciphertext-only
// password vault for a companion browser extension.
package ccds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/c/just-talk-go/config"
	"github.com/c/just-talk-go/engine"
)

type Plugin struct {
	logger *slog.Logger
	cfg    config.CCDSConfig
	reload chan config.CCDSConfig
}

func NewCCDSPlugin() *Plugin {
	return &Plugin{reload: make(chan config.CCDSConfig, 1)}
}

func (p *Plugin) Name() string    { return "ccds" }
func (p *Plugin) Version() string { return "0.1.0" }

func (p *Plugin) Init(env engine.PluginEnv) error {
	p.logger = env.Logger()
	p.cfg = env.Config().CCDS
	return nil
}

func (p *Plugin) OnConfigReload(cfg *config.Config) error {
	select {
	case <-p.reload:
	default:
	}
	p.reload <- cfg.CCDS
	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	cfg := p.cfg
	cur := p.serve(cfg)
	for {
		select {
		case <-ctx.Done():
			cur.shutdown()
			return ctx.Err()
		case next := <-p.reload:
			if next != cfg {
				cur.shutdown()
				cfg = next
				cur = p.serve(cfg)
			}
		}
	}
}

func (p *Plugin) Stop() error { return nil }

type running struct {
	srv     *http.Server
	workdir string
}

// shutdown waits briefly for in-flight requests, then drops them; dropping
// a request cancels its claude process.
func (r *running) shutdown() {
	if r == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if r.srv.Shutdown(ctx) != nil {
		r.srv.Close()
	}
	os.RemoveAll(r.workdir)
}

// serve returns nil when disabled or when the server could not start.
func (p *Plugin) serve(cfg config.CCDSConfig) *running {
	if !cfg.Enabled {
		return nil
	}
	r, err := p.listen(cfg)
	if err != nil {
		p.logger.Error("ccds failed to start", "error", err)
		return nil
	}
	return r
}

func (p *Plugin) listen(cfg config.CCDSConfig) (*running, error) {
	vaultDir, err := expandHome(cfg.VaultDir)
	if err != nil {
		return nil, err
	}
	v := &vault{dir: vaultDir}
	if err := v.loadToken(); err != nil {
		return nil, fmt.Errorf("vault token: %w", err)
	}
	workdir, err := os.MkdirTemp("", "cc-ds-")
	if err != nil {
		return nil, err
	}
	freePort(cfg.Port, p.logger)
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		os.RemoveAll(workdir)
		return nil, err
	}
	s := &server{
		cfg: settings{apiKey: cfg.APIKey, chat: cfg.Model, reasoner: cfg.Reasoner},
		claude: &claudeRunner{
			bin:     cfg.Claude,
			timeout: time.Duration(cfg.Timeout) * time.Second,
			workdir: workdir,
		},
		vault:  v,
		logger: p.logger,
	}
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.logger.Error("ccds server stopped", "error", err)
		}
	}()
	p.logger.Info("ccds listening", "url", "http://"+addr,
		"deepseek-chat", cfg.Model, "deepseek-reasoner", cfg.Reasoner,
		"vault", v.file(), "token", v.tokenFile())
	return &running{srv: srv, workdir: workdir}, nil
}

func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, path[1:]), nil
}

// freePort SIGTERMs whatever else listens on port and SIGKILLs it after 3s,
// so a previous instance (e.g. the old cc-ds.py) does not block startup.
func freePort(port int, logger *slog.Logger) {
	// lsof exits 1 when nothing listens; the empty output is all that matters.
	out, _ := exec.Command("lsof", "-t", "-nP", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN").Output()
	pids := map[int]bool{}
	for f := range strings.FieldsSeq(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil && pid != os.Getpid() {
			pids[pid] = true
		}
	}
	for pid := range pids {
		name, _ := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
		logger.Warn("ccds port in use, killing", "port", port, "pid", pid, "command", strings.TrimSpace(string(name)))
		if syscall.Kill(pid, syscall.SIGTERM) != nil {
			delete(pids, pid)
		}
	}
	for deadline := time.Now().Add(3 * time.Second); len(pids) > 0 && time.Now().Before(deadline); {
		time.Sleep(100 * time.Millisecond)
		for pid := range pids {
			if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				delete(pids, pid)
			}
		}
	}
	for pid := range pids {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}
