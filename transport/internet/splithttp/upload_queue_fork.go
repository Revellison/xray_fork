package splithttp

import (
	"context"
	"net/http"
	"sync/atomic"

	"github.com/xtls/xray-core/common/errors"
)

// maxConcurrentPostReads bounds how many POST bodies one packet-up session reads at once.
const maxConcurrentPostReads = 4

var postReadLimitEnabled atomic.Bool

// EnablePostReadLimit turns the limit on. It is set only when the speed limiter can
// throttle uploads. Without it, sessions behave exactly as they do upstream.
func EnablePostReadLimit() {
	postReadLimitEnabled.Store(true)
}

func newPostSlots() chan struct{} {
	if postReadLimitEnabled.Load() {
		return make(chan struct{}, maxConcurrentPostReads)
	}
	return nil
}

// enterPostRead waits for a slot before a POST body is read, and reports whether the
// handler should continue. Unread bodies are left to transport flow control, which slows
// this session's client alone. Otherwise a slow reader, such as the speed limiter, lets
// blocked POSTs pile up until they use up the connection's streams, which stalls every
// other session sharing that connection. Clients need no changes.
func (h *uploadQueue) enterPostRead(writer http.ResponseWriter, request *http.Request) bool {
	if h.postSlots == nil {
		return true
	}
	select {
	case h.postSlots <- struct{}{}:
		return true
	case <-h.closed.Wait():
		errors.LogInfo(context.Background(), "failed to upload: packet queue closed")
	case <-request.Context().Done():
		errors.LogInfoInner(context.Background(), request.Context().Err(), "failed to upload")
	}
	writer.WriteHeader(http.StatusInternalServerError)
	return false
}

func (h *uploadQueue) leavePostRead() {
	if h.postSlots != nil {
		<-h.postSlots
	}
}
