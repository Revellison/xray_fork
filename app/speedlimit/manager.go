package speedlimit

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/common/log"
)

var (
	globalManager     *Manager
	globalManagerOnce sync.Once
)

type UserLimiter struct {
	email string
	down  *dir
	up    *dir
	m     *Manager
}

type Manager struct {
	clk         Clock
	mu          sync.RWMutex
	cfg         *Config
	users       map[string]*UserLimiter
	ticker      *time.Ticker
	tickDone    chan struct{}
	ctx         context.Context
	cancel      context.CancelFunc
	udpMaxDelay atomic.Int64 // в миллисекундах
}

// Default возвращает глобальный менеджер. nil, если встроенный конфиг не задан.
func Default() *Manager {
	globalManagerOnce.Do(func() {
		cfg := GetEmbeddedConfig()
		if cfg == nil {
			return
		}

		globalManager = newManager(cfg)
		log.Record(&log.GeneralMessage{
			Severity: log.Severity_Info,
			Content:  "speedlimit: initialized",
		})
	})
	return globalManager
}

func newManager(cfg *Config) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		clk:      realClock{},
		cfg:      cfg,
		users:    make(map[string]*UserLimiter),
		ticker:   time.NewTicker(time.Second),
		tickDone: make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
	}
	m.udpMaxDelay.Store(int64(cfg.UDPMaxDelayMs))

	go m.tickLoop()

	return m
}

func (m *Manager) tickLoop() {
	defer close(m.tickDone)
	for {
		select {
		case <-m.ticker.C:
			m.tick()
		case <-m.ctx.Done():
			return
		}
	}
}

func (m *Manager) tick() {
	m.mu.RLock()
	snapshot := make([]*UserLimiter, 0, len(m.users))
	for _, u := range m.users {
		snapshot = append(snapshot, u)
	}
	m.mu.RUnlock()

	for _, u := range snapshot {
		u.down.tick()
		u.up.tick()
	}
}

// User возвращает UserLimiter для email. nil, если лимитер выключен или email пустой.
func (m *Manager) User(email string) *UserLimiter {
	if m == nil {
		return nil
	}

	if !m.cfg.Enabled {
		return nil
	}

	normed := normEmail(email)
	if normed == "" {
		return nil
	}

	m.mu.RLock()
	u, ok := m.users[normed]
	m.mu.RUnlock()
	if ok {
		return u
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Двойная проверка
	if u, ok := m.users[normed]; ok {
		return u
	}

	rule, ok := m.cfg.ByEmail[normed]
	if !ok {
		rule = m.cfg.Default
	}

	u = &UserLimiter{
		email: normed,
		down:  newDir(m.clk),
		up:    newDir(m.clk),
		m:     m,
	}

	u.down.apply(rule.downKbps, rule.downProf, m.cfg.Penalties, m.cfg.BurstMs)
	u.up.apply(rule.upKbps, rule.upProf, m.cfg.Penalties, m.cfg.BurstMs)

	m.users[normed] = u
	return u
}

func (m *Manager) udpMaxDelayDuration() time.Duration {
	return time.Duration(m.udpMaxDelay.Load()) * time.Millisecond
}

// MayLimit reports whether this user can ever be limited, now or through a penalty.
// Callers keep zero-copy paths such as splice for users for whom it returns false.
func (u *UserLimiter) MayLimit() bool {
	if u == nil {
		return false
	}
	return u.down.mayLimit() || u.up.mayLimit()
}

// Close останавливает менеджер.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}

	m.cancel()
	m.ticker.Stop()
	<-m.tickDone
	return nil
}
