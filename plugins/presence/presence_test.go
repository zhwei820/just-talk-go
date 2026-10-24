package presence

import (
	"testing"
	"time"

	"github.com/c/just-talk-go/config"
)

func TestWindowContains(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, time.Local) } // Thursday
	day := func(d, h int) time.Time { return time.Date(2026, 1, d, h, 0, 0, 0, time.Local) }
	tests := []struct {
		start, end string
		t          time.Time
		want       bool
	}{
		{"09:00", "20:00", at(8, 59), false},
		{"09:00", "20:00", at(9, 0), true},
		{"09:00", "20:00", at(19, 59), true},
		{"09:00", "20:00", at(20, 0), false},
		{"22:00", "06:00", at(23, 0), true},
		{"22:00", "06:00", at(5, 59), true},
		{"22:00", "06:00", at(12, 0), false},
		{"09:00", "20:00", day(3, 10), false}, // Saturday
		{"09:00", "20:00", day(4, 10), false}, // Sunday
		{"09:00", "20:00", day(5, 10), true},  // Monday
		{"22:00", "06:00", day(3, 1), true},   // Friday night's window
		{"22:00", "06:00", day(3, 23), false}, // Saturday night
		{"22:00", "06:00", day(5, 1), false},  // Sunday night's window
	}
	for _, tt := range tests {
		w, err := parseWindow(config.PresenceConfig{Start: tt.start, End: tt.end})
		if err != nil {
			t.Fatal(err)
		}
		if got := w.contains(tt.t); got != tt.want {
			t.Errorf("%s-%s at %s = %v, want %v", tt.start, tt.end, tt.t.Format("15:04"), got, tt.want)
		}
	}
}

func TestParseWindowRejectsBadInput(t *testing.T) {
	for _, pc := range []config.PresenceConfig{
		{Start: "9am", End: "20:00"},
		{Start: "09:00", End: "25:00"},
		{Start: "09:00", End: "09:00"},
	} {
		if _, err := parseWindow(pc); err == nil {
			t.Errorf("parseWindow(%+v) = nil error", pc)
		}
	}
}
