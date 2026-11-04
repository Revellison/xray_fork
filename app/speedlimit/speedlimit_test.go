package speedlimit

import (
	"testing"
	"time"
)

type fakeClock struct {
	ns int64
}

func (c *fakeClock) NowNanos() int64 { return c.ns }
func (c *fakeClock) advance(d time.Duration) {
	c.ns += int64(d)
}

func TestBucketUnlimited(t *testing.T) {
	clk := &fakeClock{}
	b := newBucket(clk)
	b.setRate(0, 20)
	if !b.unlimited.Load() {
		t.Fatal("unlimited should be true")
	}
	if !b.tryTakeNow(1000000) {
		t.Fatal("unlimited bucket should allow any size")
	}
}

func TestBucketBasic(t *testing.T) {
	clk := &fakeClock{}
	b := newBucket(clk)
	// 10 Mbps = 1_250_000 bytes/sec, burst = 20 ms -> 25_000 bytes
	b.setRate(1250000, 20)
	if b.burst != 25000 {
		t.Fatalf("expected burst 25000, got %v", b.burst)
	}

	// Should have full burst initially
	if !b.tryTakeNow(25000) {
		t.Fatal("should have initial burst")
	}

	// Empty now
	if b.tryTakeNow(100) {
		t.Fatal("should be empty after taking burst")
	}

	// Advance 1 second, should refill to burst cap
	clk.advance(time.Second)
	if !b.tryTakeNow(25000) {
		t.Fatal("should refill after 1s")
	}
}

func TestBucketMinimumBurst(t *testing.T) {
	clk := &fakeClock{}
	b := newBucket(clk)
	// 64 kbps = 8000 bytes/sec, 20 ms burst = 160 bytes -> clamped to 16 KiB
	b.setRate(8000, 20)
	if b.burst != minBurstBytes {
		t.Fatalf("expected burst clamped to %d, got %v", minBurstBytes, b.burst)
	}
}

func TestWindowBasic(t *testing.T) {
	w := newWindow(60) // 60 seconds, 1 bucket per second
	if w.n != 60 || w.widthT != 1 {
		t.Fatalf("expected 60x1, got %dx%d", w.n, w.widthT)
	}

	// Feed 59 ticks, window not full
	for i := 0; i < 59; i++ {
		w.feed(1000)
	}
	if w.avgKbps() != 0 {
		t.Fatal("incomplete window should return 0")
	}

	// 60th tick, now full
	w.feed(1000)
	avg := w.avgKbps()
	// 1000 bytes/sec = 8 kbps
	if avg != 8 {
		t.Fatalf("expected 8 kbps, got %d", avg)
	}
}

func newTestDir(clk Clock, prof *dirProfile, baseKbps int64, penalties bool) *dir {
	d := newDir(clk)
	d.apply(baseKbps, prof, penalties, 20)
	return d
}

func TestDirEscalation(t *testing.T) {
	clk := &fakeClock{}
	prof := &dirProfile{
		steps: []step{
			{aboveKbps: 8000, forTicks: 10, limitKbps: 5000},
		},
		rec: recoverCfg{afterTicks: 60},
	}
	d := newTestDir(clk, prof, 10000, true)

	// 1_500_000 bytes per tick = 12 Mbps, above the 8 Mbps threshold.
	for i := 0; i < 10; i++ {
		d.recordBytes(1500000)
		clk.advance(time.Second)
		d.tick()
	}

	if d.level != 1 {
		t.Fatalf("expected level 1, got %d", d.level)
	}
	if d.effKbps() != 5000 {
		t.Fatalf("expected 5000 kbps, got %d", d.effKbps())
	}
}

func TestDirNoEscalationBelowThreshold(t *testing.T) {
	clk := &fakeClock{}
	prof := &dirProfile{
		steps: []step{
			{aboveKbps: 8000, forTicks: 10, limitKbps: 5000},
		},
		rec: recoverCfg{afterTicks: 60},
	}
	d := newTestDir(clk, prof, 10000, true)

	// 500_000 bytes per tick = 4 Mbps, below the 8 Mbps threshold.
	for i := 0; i < 20; i++ {
		d.recordBytes(500000)
		clk.advance(time.Second)
		d.tick()
	}

	if d.level != 0 {
		t.Fatalf("expected level 0 (no penalty), got %d", d.level)
	}
	if d.effKbps() != 10000 {
		t.Fatalf("expected base 10000 kbps, got %d", d.effKbps())
	}
}

func TestDirPenaltiesGloballyDisabled(t *testing.T) {
	clk := &fakeClock{}
	prof := &dirProfile{
		steps: []step{
			{aboveKbps: 8000, forTicks: 10, limitKbps: 5000},
		},
		rec: recoverCfg{afterTicks: 60},
	}
	// penalties=false must keep the base rate no matter the throughput.
	d := newTestDir(clk, prof, 10000, false)

	for i := 0; i < 30; i++ {
		d.recordBytes(1500000)
		clk.advance(time.Second)
		d.tick()
	}

	if d.level != 0 {
		t.Fatalf("expected level 0 with penalties disabled, got %d", d.level)
	}
	if d.effKbps() != 10000 {
		t.Fatalf("expected base 10000 kbps, got %d", d.effKbps())
	}
}

func TestDirRecoverAfter(t *testing.T) {
	clk := &fakeClock{}
	prof := &dirProfile{
		steps: []step{
			{aboveKbps: 8000, forTicks: 10, limitKbps: 5000},
		},
		rec: recoverCfg{afterTicks: 30},
	}
	d := newTestDir(clk, prof, 10000, true)

	for i := 0; i < 10; i++ {
		d.recordBytes(1500000)
		clk.advance(time.Second)
		d.tick()
	}
	if d.level != 1 {
		t.Fatalf("expected level 1, got %d", d.level)
	}

	// Idle long enough for recover.after to fire.
	for i := 0; i < 31; i++ {
		clk.advance(time.Second)
		d.tick()
	}
	if d.level != 0 {
		t.Fatalf("expected level 0 after recover.after, got %d", d.level)
	}
	if d.effKbps() != 10000 {
		t.Fatalf("expected base 10000 kbps after recovery, got %d", d.effKbps())
	}
}

// TestConfigParseDocumentedSchema parses the exact example that ships in
// docs/SPEEDLIMIT.md, so the docs cannot silently drift from the parser.
func TestConfigParseDocumentedSchema(t *testing.T) {
	data := []byte(`{
		"enabled": true,
		"penalties": true,
		"burstMs": 20,
		"udp": {"maxDelayMs": 100},
		"default": {"down": 50000, "up": 20000, "penalty": "standard"},
		"profiles": {
			"standard": {
				"down": {
					"steps": [{"above": 45000, "for": "5m", "limit": 25000}],
					"recover": {"after": "2h"}
				},
				"up": {
					"steps": [{"above": 18000, "for": "3m", "limit": 8000}],
					"recover": {"after": "1h"}
				}
			}
		},
		"users": [
			{"emails": ["VIP@Example.com"], "down": 100000, "up": 100000, "penalty": "none"}
		]
	}`)

	cfg, _, err := Parse(data)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if cfg.Default.downKbps != 50000 || cfg.Default.upKbps != 20000 {
		t.Fatalf("wrong default limits: %d/%d", cfg.Default.downKbps, cfg.Default.upKbps)
	}
	if cfg.Default.downProf == nil || cfg.Default.upProf == nil {
		t.Fatal("default rule must reference the standard profile for both directions")
	}
	if got := cfg.Default.downProf.steps[0].limitKbps; got != 25000 {
		t.Fatalf("wrong down penalty limit: %d", got)
	}

	rule, ok := cfg.ByEmail["vip@example.com"]
	if !ok {
		t.Fatalf("emails must be normalized to lower case, got keys %v", keysOf(cfg.ByEmail))
	}
	if rule.downKbps != 100000 || rule.upKbps != 100000 {
		t.Fatalf("wrong vip limits: %d/%d", rule.downKbps, rule.upKbps)
	}
	if rule.downProf != nil || rule.upProf != nil {
		t.Fatal(`penalty "none" must not attach any profile`)
	}
}

func keysOf(m map[string]resolvedRule) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestConfigParseBasic(t *testing.T) {
	data := []byte(`{
		"enabled": true,
		"default": {"down": 10000, "up": 5000}
	}`)

	cfg, warns, err := Parse(data)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(warns) > 0 {
		t.Logf("warnings: %v", warns)
	}
	if !cfg.Enabled {
		t.Fatal("enabled should be true")
	}
	if cfg.Default.downKbps != 10000 || cfg.Default.upKbps != 5000 {
		t.Fatalf("wrong limits: %d/%d", cfg.Default.downKbps, cfg.Default.upKbps)
	}
}

func TestConfigValidationV05(t *testing.T) {
	data := []byte(`{
		"enabled": true,
		"default": {"down": 30, "up": 5000}
	}`)

	_, _, err := Parse(data)
	if err == nil {
		t.Fatal("should reject down=30 (V05)")
	}
	if !contains(err.Error(), "must be >= 64") {
		t.Fatalf("wrong error: %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || findInString(s, substr)))
}

func findInString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
