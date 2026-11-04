package speedlimit

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

const (
	minBurstBytes = 16 * 1024 // не меньше двух стандартных буферов (buf.Size = 8192)
	minSleep      = 300 * time.Microsecond
	maxSleep      = 250 * time.Millisecond
)

type bucket struct {
	clk       Clock
	mu        sync.Mutex
	rate      float64 // байт/с, 0 = безлимит
	burst     float64
	tokens    float64
	lastNs    int64
	unlimited atomic.Bool // зеркало rate==0 для быстрого пути без мьютекса
}

func newBucket(clk Clock) *bucket {
	b := &bucket{clk: clk}
	b.unlimited.Store(true)
	return b
}

func (b *bucket) setRate(bytesPerSec float64, burstMs int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clk.NowNanos()
	if b.rate > 0 {
		b.refillLocked(now)
	}
	wasUnlimited := b.rate <= 0
	b.rate = bytesPerSec
	if bytesPerSec <= 0 {
		b.unlimited.Store(true)
		b.lastNs = now
		return
	}
	b.burst = bytesPerSec * float64(burstMs) / 1000
	if b.burst < minBurstBytes {
		b.burst = minBurstBytes
	}
	if wasUnlimited {
		b.tokens = b.burst // старт с полной корзиной
	} else if b.tokens > b.burst {
		b.tokens = b.burst // только обрезаем, никогда не уходим в минус
	}
	b.lastNs = now
	b.unlimited.Store(false)
}

func (b *bucket) refillLocked(now int64) {
	if dt := now - b.lastNs; dt > 0 {
		b.tokens += float64(dt) / 1e9 * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
	}
	b.lastNs = now
}

// tryTake забирает n токенов, если они уже есть. Иначе ничего не забирает и
// возвращает, сколько примерно подождать. n>burst обрезается до burst
// (защита от вечного ожидания).
func (b *bucket) tryTake(n int) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rate <= 0 {
		return 0
	}
	b.refillLocked(b.clk.NowNanos())
	need := float64(n)
	if need > b.burst {
		need = b.burst
	}
	if b.tokens >= need {
		b.tokens -= need
		return 0
	}
	w := time.Duration((need - b.tokens) / b.rate * 1e9)
	if w < minSleep {
		w = minSleep
	}
	if w > maxSleep {
		w = maxSleep
	}
	// джиттер +0..10%, чтобы проснувшиеся горутины не бились за токены в одну микросекунду
	w += time.Duration(rand.Int64N(int64(w)/10 + 1))
	return w
}

// tryTakeNow пытается взять n токенов прямо сейчас. Возвращает true, если успешно.
func (b *bucket) tryTakeNow(n int) bool {
	if b.unlimited.Load() {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rate <= 0 {
		return true
	}
	b.refillLocked(b.clk.NowNanos())
	need := float64(n)
	if need > b.burst {
		need = b.burst
	}
	if b.tokens >= need {
		b.tokens -= need
		return true
	}
	return false
}
