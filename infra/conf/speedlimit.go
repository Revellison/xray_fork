package conf

import (
	"encoding/json"

	"github.com/xtls/xray-core/app/speedlimit"
	"github.com/xtls/xray-core/common/errors"
)

// SpeedlimitConfig is the "speedLimit" section of the top-level config file.
//
// It is kept as raw JSON on purpose: the whole section is handed over to
// speedlimit.Parse, which is the single source of truth for the schema and
// for validation rules V01..V27. That way the in-config section and the
// documentation can never drift apart.
//
// Example:
//
//	"speedLimit": {
//	  "enabled": true,
//	  "penalties": true,
//	  "default": { "down": 50000, "up": 20000, "penalty": "standard" },
//	  "profiles": {
//	    "standard": {
//	      "down": { "steps": [{ "above": 45000, "for": "5m", "limit": 25000 }], "recover": { "after": "2h" } },
//	      "up":   { "steps": [{ "above": 18000, "for": "3m", "limit": 8000 }],  "recover": { "after": "1h" } }
//	    }
//	  },
//	  "users": [ { "emails": ["vip@example.com"], "down": 100000, "up": 100000, "penalty": "none" } ]
//	}
type SpeedlimitConfig struct {
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON records the raw section and lets speedlimit.Parse interpret it.
func (c *SpeedlimitConfig) UnmarshalJSON(data []byte) error {
	c.Raw = append(c.Raw[:0], data...)
	return nil
}

// MarshalJSON restores the raw section verbatim.
func (c *SpeedlimitConfig) MarshalJSON() ([]byte, error) {
	if len(c.Raw) == 0 {
		return []byte("null"), nil
	}
	return c.Raw, nil
}

// Build parses and validates the section into a runtime *speedlimit.Config.
func (c *SpeedlimitConfig) Build() (*speedlimit.Config, error) {
	if c == nil || len(c.Raw) == 0 {
		return nil, nil
	}

	cfg, warnings, err := speedlimit.Parse(c.Raw)
	if err != nil {
		return nil, errors.New("invalid speedLimit section").Base(err)
	}

	for _, w := range warnings {
		errors.LogWarning(nil, "speedLimit: ", w)
	}

	return cfg, nil
}
