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
	nudgeSettle   = 2 * time.Second
)

type window struct {
	start, end int // minutes since local midnight
}

// contains treats end before start as a window spanning midnight. Only
// windows starting on a weekday count, so Friday night's window still runs
// into Saturday morning while Sunday night's into Monday does not.
func (w window) contains(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	startDay := t
	var in bool
	if w.start <= w.end {
		in = m >= w.start && m < w.end
	} else {
		in = m >= w.start || m < w.end
		if m < w.end {
			startDay = t.AddDate(0, 0, -1)
		}
	}
	wd := startDay.Weekday()
	return in && wd != time.Saturday && wd != time.Sunday
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

// TUIPresenceStatus is what the TUI shows; an empty State hides the line.
type TUIPresenceStatus struct {
	State     string // "active", "outside", "failed", "invalid"
	Detail    string
	LastNudge time.Time
}

var (
	tuiStatusMu sync.Mutex
	tuiStatus   TUIPresenceStatus
)

func TUIStatus() TUIPresenceStatus {
	tuiStatusMu.Lock()
	defer tuiStatusMu.Unlock()
	return tuiStatus
}

func setTUIStatus(update func(*TUIPresenceStatus)) {
	tuiStatusMu.Lock()
	defer tuiStatusMu.Unlock()
	update(&tuiStatus)
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
	setTUIStatus(func(s *TUIPresenceStatus) { *s = TUIPresenceStatus{} })
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
		setTUIStatus(func(s *TUIPresenceStatus) { s.State, s.Detail = "invalid", err.Error() })
		return
	}
	p.enabled, p.win = true, w
	p.logger.Info("presence enabled", "start", pc.Start, "end", pc.End)
	state := "outside"
	if w.contains(time.Now()) {
		state = "active"
	}
	setTUIStatus(func(s *TUIPresenceStatus) { s.State, s.Detail = state, pc.Start+"–"+pc.End })
}

func (p *Plugin) Start(ctx context.Context) error {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		t.Reset(p.tick(time.Now()))
	}
}

func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.setAwake(false)
	return nil
}

// NextNudgeIn is how long until idle time reaches the nudge threshold.
func NextNudgeIn() time.Duration {
	return max(idleThreshold-idleTime(), 0)
}

// tick returns the delay until the next check: when the idle threshold will
// be reached, capped so window and config changes are still noticed.
func (p *Plugin) tick(now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.enabled {
		p.setAwake(false)
		return checkInterval
	}
	active := p.win.contains(now)
	p.setAwake(active)
	if !active {
		setTUIStatus(func(s *TUIPresenceStatus) { s.State = "outside" })
		return checkInterval
	}
	if left := NextNudgeIn(); left > 0 {
		// Keep a reported failure until a nudge proves otherwise.
		if !p.warnedNudge {
			setTUIStatus(func(s *TUIPresenceStatus) { s.State = "active" })
		}
		return min(left, checkInterval)
	}
	// A synthetic event that leaves idle time untouched was dropped, which
	// means the launching terminal lacks the Accessibility permission.
	if !nudgeAndWait() {
		if !p.warnedNudge {
			p.logger.Warn("presence could not reset idle time; grant Accessibility to the terminal running Just Talk")
			p.warnedNudge = true
		}
		setTUIStatus(func(s *TUIPresenceStatus) { s.State = "failed" })
		return checkInterval
	}
	p.warnedNudge = false
	setTUIStatus(func(s *TUIPresenceStatus) { s.State, s.LastNudge = "active", now })
	p.logger.Debug("presence reset idle time")
	return checkInterval
}

// nudgeAndWait reports whether the nudge reset idle time. macOS reflects a
// posted event in idle time only a few hundred milliseconds later.
func nudgeAndWait() bool {
	nudge()
	deadline := time.Now().Add(nudgeSettle)
	for {
		if idleTime() < idleThreshold {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
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
