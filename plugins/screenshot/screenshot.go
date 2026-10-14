// Package screenshot captures a mouse-selected screen region to the clipboard.
package screenshot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/c/just-talk-go/config"
	"github.com/c/just-talk-go/engine"
	"github.com/c/just-talk-go/hotkey"
)

// errCancelled means the user aborted the selection (Esc / right click).
var errCancelled = errors.New("screenshot cancelled")

type Plugin struct {
	env    engine.PluginEnv
	logger *slog.Logger

	mu         sync.Mutex
	combo      hotkey.Combo
	registered bool

	busy atomic.Bool
}

func NewScreenshotPlugin() *Plugin { return &Plugin{} }

func (p *Plugin) Name() string    { return "screenshot" }
func (p *Plugin) Version() string { return "0.1.0" }

func (p *Plugin) Init(env engine.PluginEnv) error {
	p.env = env
	p.logger = env.Logger()
	// A bad screenshot hotkey must not keep voice input from starting.
	if err := p.registerFromConfig(env.Config()); err != nil {
		p.logger.Warn("screenshot hotkey unavailable", "error", err)
	}
	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (p *Plugin) Stop() error { return nil }

func (p *Plugin) OnConfigReload(cfg *config.Config) error { return p.registerFromConfig(cfg) }

func (p *Plugin) registerFromConfig(cfg *config.Config) error {
	sc := cfg.Screenshot
	var combo hotkey.Combo
	if sc.Enabled {
		c, err := config.ParseHotkey(sc.Hotkey)
		if err != nil {
			return fmt.Errorf("parse screenshot hotkey %q: %w", sc.Hotkey, err)
		}
		combo = c
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.registered && sc.Enabled && p.combo == combo {
		return nil
	}
	if p.registered {
		p.env.UnregisterHotkey(p.combo)
		p.registered = false
	}
	if !sc.Enabled {
		p.logger.Info("screenshot disabled")
		return nil
	}
	if err := p.env.RegisterHotkeyWithOptions(combo, hotkey.RegisterOptions{Suppress: true}, p.onHotkey); err != nil {
		return fmt.Errorf("register screenshot hotkey %s: %w", combo, err)
	}
	p.combo = combo
	p.registered = true
	p.logger.Info("screenshot hotkey registered", "combo", combo)
	return nil
}

func (p *Plugin) onHotkey(evt hotkey.Event) {
	if evt.Type != hotkey.KeyDown {
		return
	}
	// The selection UI is modal; a second press while it is open is ignored.
	if !p.busy.CompareAndSwap(false, true) {
		p.logger.Debug("screenshot already in progress")
		return
	}
	go func() {
		defer p.busy.Store(false)
		start := time.Now()
		err := captureRegionToClipboard(p.env.Engine().Context(), p.logger)
		switch {
		case errors.Is(err, errCancelled):
			p.logger.Info("screenshot cancelled")
		case err != nil:
			p.logger.Warn("screenshot failed", "error", err)
		default:
			p.logger.Info("screenshot finished", "elapsed", time.Since(start).Round(time.Millisecond))
		}
	}()
}
