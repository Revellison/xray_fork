package conf_test

import (
	"encoding/json"
	"testing"

	"github.com/xtls/xray-core/app/speedlimit"
	"github.com/xtls/xray-core/infra/conf"
)

// TestSpeedLimitSectionBuilds proves the "speedLimit" section placed at the
// root of config.json is parsed, validated and installed into the speedlimit
// package at config-build time.
func TestSpeedLimitSectionBuilds(t *testing.T) {
	raw := `{
		"log": {"loglevel": "warning"},
		"inbounds": [{
			"port": 1080,
			"protocol": "socks",
			"settings": {"auth": "noauth", "udp": true}
		}],
		"outbounds": [{"protocol": "freedom"}],
		"speedLimit": {
			"enabled": true,
			"penalties": true,
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
				{"emails": ["vip@example.com"], "down": 100000, "up": 100000, "penalty": "none"}
			]
		}
	}`

	var c conf.Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Speedlimit == nil {
		t.Fatal("speedLimit section was not decoded")
	}
	if _, err := c.Build(); err != nil {
		t.Fatalf("build: %v", err)
	}
	if speedlimit.GetEmbeddedConfig() == nil {
		t.Fatal("speedlimit config was not installed")
	}
}

// TestSpeedLimitSectionRejectsBadValues proves validation errors abort the
// config build instead of silently degrading to "no limiter".
func TestSpeedLimitSectionRejectsBadValues(t *testing.T) {
	raw := `{
		"inbounds": [{"port": 1080, "protocol": "socks", "settings": {"auth": "noauth"}}],
		"outbounds": [{"protocol": "freedom"}],
		"speedLimit": {
			"enabled": true,
			"default": {"down": 30, "up": 5000}
		}
	}`

	var c conf.Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, err := c.Build(); err == nil {
		t.Fatal("expected an error for down=30 (< 64 kbps), got nil")
	}
}

// TestSpeedLimitSectionAbsent keeps the feature strictly opt-in.
func TestSpeedLimitSectionAbsent(t *testing.T) {
	raw := `{
		"inbounds": [{"port": 1080, "protocol": "socks", "settings": {"auth": "noauth"}}],
		"outbounds": [{"protocol": "freedom"}]
	}`

	var c conf.Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Speedlimit != nil {
		t.Fatal("speedLimit must stay nil when the section is absent")
	}
	if _, err := c.Build(); err != nil {
		t.Fatalf("build: %v", err)
	}
}
