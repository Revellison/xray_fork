package speedlimit

import (
	"context"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
)

// Reader limits uplink traffic on links that hand the client's reader straight
// to the outbound (routing.Dispatcher.DispatchLink), where no uplink writer exists.
// Data is returned in small paced batches, so a large read does not leave as a burst.
type Reader struct {
	ctx     context.Context
	inner   buf.Reader
	d       *dir
	udp     bool
	m       *Manager
	pending buf.MultiBuffer
	err     error
}

var _ buf.Reader = (*Reader)(nil)

// WrapUpReader limits an uplink reader. It is the reader counterpart of WrapUp.
func (u *UserLimiter) WrapUpReader(ctx context.Context, r buf.Reader, n net.Network) buf.Reader {
	if u == nil {
		return r
	}
	return &Reader{ctx: ctx, inner: r, d: u.up, udp: n == net.Network_UDP, m: u.m}
}

// ReadMultiBuffer implements buf.Reader.
func (r *Reader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	for {
		if r.pending.IsEmpty() {
			if r.err != nil {
				return nil, r.err
			}
			r.pending, r.err = r.inner.ReadMultiBuffer()
			if r.pending.IsEmpty() {
				err := r.err
				r.pending, r.err = nil, nil
				return nil, err
			}
		}

		if r.d.bkt.unlimited.Load() {
			mb := r.pending
			r.pending = nil
			r.d.recordBytes(mb.Len())
			return mb, nil
		}

		if r.udp {
			mb, err := r.nextUDP()
			if err != nil {
				return nil, err
			}
			if mb.IsEmpty() {
				continue // everything was dropped; read more
			}
			return mb, nil
		}
		return r.nextTCP()
	}
}

func (r *Reader) nextTCP() (buf.MultiBuffer, error) {
	first := int(r.pending[0].Len())
	if err := waitTokens(r.ctx, nil, r.d, first); err != nil {
		r.releasePending()
		return nil, err
	}
	sent, n := first, 1
	for n < len(r.pending) && r.d.bkt.tryTakeNow(int(r.pending[n].Len())) {
		sent += int(r.pending[n].Len())
		n++
	}
	mb := r.pending[:n:n]
	r.pending = r.pending[n:]
	r.d.recordBytes(int32(sent))
	return mb, nil
}

func (r *Reader) nextUDP() (buf.MultiBuffer, error) {
	maxDelay := r.m.udpMaxDelayDuration()
	for !r.pending.IsEmpty() {
		b := r.pending[0]
		r.pending = r.pending[1:]
		admitted, err := admitUDP(r.ctx, nil, r.d, int(b.Len()), maxDelay)
		if err != nil {
			b.Release()
			r.releasePending()
			return nil, err
		}
		if admitted {
			r.d.recordBytes(b.Len())
			return buf.MultiBuffer{b}, nil
		}
		b.Release()
	}
	return nil, nil
}

func (r *Reader) releasePending() {
	buf.ReleaseMulti(r.pending)
	r.pending = nil
}
