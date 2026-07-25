package database

import (
	"testing"
	"time"
)

func TestSetTimeoutParam(t *testing.T) {
	tests := []struct {
		name    string
		initial map[string]string
		d       time.Duration
		want    string
	}{
		{"sets milliseconds", map[string]string{}, 30 * time.Second, "30000"},
		{"sub-second value", map[string]string{}, 250 * time.Millisecond, "250"},
		{"zero leaves the server default", map[string]string{}, 0, ""},
		{"negative leaves the server default", map[string]string{}, -time.Second, ""},
		{
			// A value in the DSN is the operator being explicit and must win.
			name:    "existing DSN value wins",
			initial: map[string]string{"statement_timeout": "1234"},
			d:       30 * time.Second,
			want:    "1234",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := tt.initial
			setTimeoutParam(params, "statement_timeout", tt.d)
			if got := params["statement_timeout"]; got != tt.want {
				t.Errorf("statement_timeout = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOrDuration(t *testing.T) {
	if got := orDuration(0, 5*time.Second); got != 5*time.Second {
		t.Errorf("zero should fall back, got %v", got)
	}
	if got := orDuration(-1, 5*time.Second); got != 5*time.Second {
		t.Errorf("negative should fall back, got %v", got)
	}
	if got := orDuration(time.Second, 5*time.Second); got != time.Second {
		t.Errorf("explicit value should win, got %v", got)
	}
}

func TestPoolStatsNilPool(t *testing.T) {
	if got := PoolStats(nil); got != (Stats{}) {
		t.Errorf("PoolStats(nil) = %+v, want the zero value", got)
	}
}
