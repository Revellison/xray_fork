package speedlimit

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
)

type Writer struct {
	ctx   context.Context
	inner buf.Writer
	d     *dir
	udp   bool
	m     *Manager
	done  chan struct{}
	once  sync.Once
}

var _ buf.Writer = (*Writer)(nil)

type dir struct {
	bkt    *bucket
	passed uint64 // монотонный счётчик байт

	mu         sync.Mutex
	burstMs    int
	baseKbps   int64
	prof       *dirProfile
	penaltyOn  bool
	level      int
	levelTicks int
	lastPassed uint64
	esc, rec   *window
}

func newDir(clk Clock) *dir {
	return &dir{bkt: newBucket(clk)}
}

func (u *UserLimiter) WrapUp(ctx context.Context, w buf.Writer, n net.Network) buf.Writer {
	if u == nil {
		return w
	}
	return newWriter(ctx, w, u.up, n == net.Network_UDP, u.m)
}

func (u *UserLimiter) WrapDown(ctx context.Context, w buf.Writer, n net.Network) buf.Writer {
	if u == nil {
		return w
	}
	return newWriter(ctx, w, u.down, n == net.Network_UDP, u.m)
}

func newWriter(ctx context.Context, w buf.Writer, d *dir, udp bool, m *Manager) *Writer {
	return &Writer{ctx: ctx, inner: w, d: d, udp: udp, m: m, done: make(chan struct{})}
}

func (w *Writer) Close() error {
	w.shutdown()
	return common.Close(w.inner)
}

func (w *Writer) Interrupt() {
	w.shutdown()
	common.Interrupt(w.inner)
}

func (w *Writer) shutdown() {
	w.once.Do(func() { close(w.done) })
}

func (w *Writer) WriteMultiBuffer(mb buf.MultiBuffer) error {
	d := w.d
	if d == nil || d.bkt.unlimited.Load() {
		if d != nil {
			d.recordBytes(mb.Len())
		}
		return w.inner.WriteMultiBuffer(mb)
	}
	if w.udp {
		return w.writeUDP(mb)
	}
	for len(mb) > 0 {
		first := int(mb[0].Len())
		if err := w.wait(first); err != nil {
			buf.ReleaseMulti(mb)
			return err
		}
		sent, n := first, 1
		for n < len(mb) && d.bkt.tryTakeNow(int(mb[n].Len())) {
			sent += int(mb[n].Len())
			n++
		}
		batch, rest := mb[:n:n], mb[n:]
		d.recordBytes(int32(sent))
		if err := w.inner.WriteMultiBuffer(batch); err != nil {
			buf.ReleaseMulti(rest)
			return err
		}
		mb = rest
	}
	return nil
}

func (w *Writer) wait(n int) error {
	return waitTokens(w.ctx, w.done, w.d, n)
}

// waitTokens blocks until n tokens are taken. A nil done never fires.
func waitTokens(ctx context.Context, done <-chan struct{}, d *dir, n int) error {
	for {
		dur := d.bkt.tryTake(n)
		if dur == 0 {
			return nil
		}
		t := time.NewTimer(dur)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-done:
			t.Stop()
			return io.ErrClosedPipe
		}
		if d.bkt.unlimited.Load() {
			return nil
		}
	}
}

// admitUDP waits up to maxDelay for tokens. It returns false if the datagram must be dropped.
func admitUDP(ctx context.Context, done <-chan struct{}, d *dir, size int, maxDelay time.Duration) (bool, error) {
	deadline := time.Now().Add(maxDelay)
	for {
		dur := d.bkt.tryTake(size)
		if dur == 0 {
			return true, nil
		}
		if time.Now().Add(dur).After(deadline) {
			return false, nil
		}
		t := time.NewTimer(dur)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return false, ctx.Err()
		case <-done:
			t.Stop()
			return false, io.ErrClosedPipe
		}
	}
}

func (w *Writer) writeUDP(mb buf.MultiBuffer) error {
	maxDelay := w.m.udpMaxDelayDuration()
	for i, b := range mb {
		admitted, err := admitUDP(w.ctx, w.done, w.d, int(b.Len()), maxDelay)
		if err != nil {
			// Earlier buffers were already handed downstream or released.
			buf.ReleaseMulti(mb[i:])
			return err
		}
		if !admitted {
			b.Release()
			continue
		}
		w.d.recordBytes(b.Len())
		if err := w.inner.WriteMultiBuffer(buf.MultiBuffer{b}); err != nil {
			buf.ReleaseMulti(mb[i+1:])
			return err
		}
	}
	return nil
}

func (d *dir) mayLimit() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.baseKbps > 0 || d.penaltyOn
}

func (d *dir) recordBytes(n int32) {
	d.mu.Lock()
	d.passed += uint64(n)
	d.mu.Unlock()
}

func (d *dir) tick() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	cur := d.passed
	delta := cur - d.lastPassed
	d.lastPassed = cur
	if d.prof == nil || !d.penaltyOn {
		return false
	}
	d.levelTicks++
	escDone, recDone := false, false
	if d.esc != nil {
		escDone = d.esc.feed(delta)
	}
	if d.rec != nil {
		recDone = d.rec.feed(delta)
	}
	p := d.prof

	// Снижение имеет приоритет
	if d.level >= 1 && !p.rec.never {
		byAfter := p.rec.afterTicks > 0 && d.levelTicks >= p.rec.afterTicks
		byBelow := d.rec != nil && recDone && d.rec.avgKbps() < int64(p.rec.belowKbps)
		if byAfter || byBelow {
			d.enterLevel(d.level - 1)
			return true
		}
	}

	// Эскалация
	if d.level < len(p.steps) && d.esc != nil && escDone && d.esc.avgKbps() >= int64(p.steps[d.level].aboveKbps) {
		d.enterLevel(d.level + 1)
		return true
	}
	return false
}

func (d *dir) enterLevel(l int) {
	d.level = l
	d.levelTicks = 0
	p := d.prof
	d.esc, d.rec = nil, nil
	if l < len(p.steps) {
		d.esc = newWindow(p.steps[l].forTicks)
	}
	if l >= 1 && p.rec.belowKbps > 0 {
		d.rec = newWindow(p.rec.forTicks)
	}
	d.applyRateLocked()
}

func (d *dir) applyRateLocked() {
	kbps := d.effKbps()
	d.bkt.setRate(float64(kbps)*125, d.burstMs)
}

func (d *dir) effKbps() int64 {
	if d.prof == nil || d.level == 0 {
		return d.baseKbps
	}
	lim := d.prof.steps[d.level-1].limitKbps
	if d.baseKbps == 0 || lim < d.baseKbps {
		return lim
	}
	return d.baseKbps
}

func (d *dir) apply(base int64, p *dirProfile, penalties bool, burstMs int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.burstMs, d.baseKbps, d.prof = burstMs, base, p
	d.penaltyOn = p != nil && penalties
	if !d.penaltyOn {
		d.level, d.levelTicks, d.esc, d.rec = 0, 0, nil, nil
		d.applyRateLocked()
		return
	}
	newLevel := d.level
	if newLevel > len(p.steps) {
		newLevel = len(p.steps)
	}
	d.enterLevel(newLevel)
}
