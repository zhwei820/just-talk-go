// Package presence keeps the system from going idle during configured hours,
// so chat apps that track input idle time (Slack, Teams) keep showing active.
package presence

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/c/just-talk-go/config"
	"github.com/c/just-talk-go/engine"
)

const (
	checkInterval = time.Minute
	// Slack switches to away after about 10 minutes without input.
	idleThreshold = 4 * time.Minute
)

type window struct {
	start, end int // minutes since local midnight
}

// contains treats end before start as a window spanning midnight.
func (w window) contains(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	if w.start <= w.end {
		return m >= w.start && m < w.end
	}
	return m >= w.start || m < w.end
}

func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("invalid time %q, want HH:MM", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

func parseWindow(pc config.PresenceConfig) (window, error) {
	start, err := parseClock(pc.Start)
	if err != nil {
		return window{}, err
	}
	end, err := parseClock(pc.End)
	if err != nil {
		return window{}, err
	}
	if start == end {
		return window{}, fmt.Errorf("start and end are both %s", pc.Start)
	}
	return window{start, end}, nil
}

type Plugin struct {
	logger *slog.Logger

	mu      sync.Mutex
	enabled bool
	win     window

	awake       bool
	warnedNudge bool
}

func NewPresencePlugin() *Plugin { return &Plugin{} }

func (p *Plugin) Name() string    { return "presence" }
func (p *Plugin) Version() string { return "0.1.0" }

func (p *Plugin) Init(env engine.PluginEnv) error {
	p.logger = env.Logger()
	p.apply(env.Config())
	return nil
}

func (p *Plugin) OnConfigReload(cfg *config.Config) error {
	p.apply(cfg)
	return nil
}

func (p *Plugin) apply(cfg *config.Config) {
	pc := cfg.Presence
	p.mu.Lock()
	defer p.mu.Unlock()
	p.enabled = false
	if !pc.Enabled {
		return
	}
	if !supported {
		p.logger.Warn("presence is only supported on macOS")
		return
	}
	w, err := parseWindow(pc)
	if err != nil {
		p.logger.Warn("presence disabled", "error", err)
		return
	}
	p.enabled, p.win = true, w
	p.logger.Info("presence enabled", "start", pc.Start, "end", pc.End)
}

func (p *Plugin) Start(ctx context.Context) error {
	t := time.NewTicker(checkInterval)
	defer t.Stop()
	for {
		p.tick(time.Now())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.setAwake(false)
	return nil
}

func (p *Plugin) tick(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	active := p.enabled && p.win.contains(now)
	p.setAwake(active)
	if !active || idleTime() < idleThreshold {
		return
	}
	nudge()
	// A synthetic event that leaves idle time untouched was dropped, which
	// means the launching terminal lacks the Accessibility permission.
	if idleTime() >= idleThreshold {
		if !p.warnedNudge {
			p.logger.Warn("presence could not reset idle time; grant Accessibility to the terminal running Just Talk")
			p.warnedNudge = true
		}
		return
	}
	p.warnedNudge = false
	p.logger.Debug("presence reset idle time")
}

func (p *Plugin) setAwake(on bool) {
	if p.awake == on {
		return
	}
	if err := holdAwake(on); err != nil {
		p.logger.Warn("presence sleep assertion failed", "error", err)
		return
	}
	p.awake = on
}
