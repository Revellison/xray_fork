package conf

import (
	"encoding/json"
	"fmt"

	"github.com/xtls/xray-core/app/speedlimit"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/transport/internet/splithttp"
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

// applySpeedlimitUploadDefaults keeps throttled uploads from stalling other flows on
// connections shared through HTTP/2 or QUIC. Values set in the config are left alone.
func applySpeedlimitUploadDefaults(inbounds []InboundDetourConfig, cfg *speedlimit.Config) {
	stream := cfg.UploadStreamBuffer()
	if stream == 0 {
		return
	}
	splithttp.EnablePostReadLimit()
	connection := max(16*1024*1024, 16*stream)
	for i := range inbounds {
		s := inbounds[i].StreamSetting
		if s == nil || s.Network == nil {
			continue
		}
		network, err := s.Network.Build()
		if err != nil {
			continue
		}
		switch network {
		case "splithttp":
			x := s.XHTTPSettings
			if x == nil {
				x = s.SplitHTTPSettings
			}
			if x == nil {
				x = new(SplitHTTPConfig)
				s.XHTTPSettings = x
			}
			if len(x.H2Params) == 0 {
				x.H2Params = json.RawMessage(fmt.Sprintf(
					`{"MaxReceiveBufferPerStream":%d,"MaxReceiveBufferPerConnection":%d}`, stream, connection))
			}
		case "hysteria":
		default:
			continue
		}
		if s.FinalMask == nil {
			s.FinalMask = new(FinalMask)
		}
		if s.FinalMask.QuicParams == nil {
			s.FinalMask.QuicParams = new(QuicParamsConfig)
		}
		q := s.FinalMask.QuicParams
		if q.InitStreamReceiveWindow == 0 {
			// Short-lived packet-up streams would never grow a smaller starting window.
			q.InitStreamReceiveWindow = uint64(stream)
		}
		if q.MaxStreamReceiveWindow == 0 {
			q.MaxStreamReceiveWindow = uint64(stream)
		}
		if q.InitConnectionReceiveWindow == 0 {
			q.InitConnectionReceiveWindow = uint64(connection)
		}
		if q.MaxConnectionReceiveWindow == 0 {
			q.MaxConnectionReceiveWindow = uint64(connection)
		}
	}
}
