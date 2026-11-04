package speedlimit

import (
	"sync"

	"github.com/xtls/xray-core/common/errors"
)

var (
	embeddedConfig   *Config
	embeddedConfigMu sync.RWMutex
)

// SetEmbeddedConfig устанавливает встроенный конфиг из основного config.json Xray.
// Вызывается из core при старте, если в конфиге есть секция speedlimit.
func SetEmbeddedConfig(cfg *Config) error {
	if cfg == nil {
		return errors.New("speedlimit config is nil")
	}

	embeddedConfigMu.Lock()
	embeddedConfig = cfg
	embeddedConfigMu.Unlock()

	return nil
}

// GetEmbeddedConfig возвращает встроенный конфиг, если он был установлен.
func GetEmbeddedConfig() *Config {
	embeddedConfigMu.RLock()
	defer embeddedConfigMu.RUnlock()
	return embeddedConfig
}

// ParseFromJSON разбирает JSON-конфиг speedlimit из основного config.json.
// Возвращает готовый Config, предупреждения и ошибки.
func ParseFromJSON(data []byte) (*Config, []string, error) {
	return Parse(data)
}
