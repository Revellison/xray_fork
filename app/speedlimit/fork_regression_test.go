package speedlimit

import (
	"testing"
	"time"
)

// A value of N in udp.maxDelayMs must mean N milliseconds. It was once read as nanoseconds,
// which dropped UDP instead of delaying it.
func TestUDPMaxDelayIsMilliseconds(t *testing.T) {
	m := &Manager{}
	m.udpMaxDelay.Store(100)
	if got := m.udpMaxDelayDuration(); got != 100*time.Millisecond {
		t.Fatalf("got %v, want 100ms", got)
	}
}

func TestUploadStreamBuffer(t *testing.T) {
	rule := func(up int64) resolvedRule { return resolvedRule{upKbps: up} }
	tests := []struct {
		name string
		cfg  *Config
		want int64
	}{
		{"nil config", nil, 0},
		{"disabled", &Config{Default: rule(8000)}, 0},
		{"nobody limited", &Config{Enabled: true}, 0},
		{"slow limit uses the minimum", &Config{Enabled: true, Default: rule(8000)}, minUploadStreamBuffer},
		{"scales with the limit", &Config{Enabled: true, Default: rule(50000)}, 625000},
		{"fast limit uses the maximum", &Config{Enabled: true, Default: rule(1000000)}, maxUploadStreamBuffer},
		{
			"an unlimited user keeps the maximum",
			&Config{Enabled: true, Default: rule(8000), ByEmail: map[string]resolvedRule{"vip": rule(0)}},
			maxUploadStreamBuffer,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.UploadStreamBuffer(); got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}

// A user who can never be limited must stay on upstream's path, splice included.
func TestMayLimit(t *testing.T) {
	var none *UserLimiter
	if none.MayLimit() {
		t.Fatal("nil limiter must not limit")
	}
	clk := &fakeClock{}
	unlimited := &UserLimiter{down: newDir(clk), up: newDir(clk)}
	if unlimited.MayLimit() {
		t.Fatal("unlimited user must not limit")
	}
	limited := &UserLimiter{down: newDir(clk), up: newDir(clk)}
	limited.up.apply(8000, nil, false, 20)
	if !limited.MayLimit() {
		t.Fatal("user with an upload limit must limit")
	}
}
