package stream

import (
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

// Retransmissions answer NACKs with packets already sent. On a congested link the resends are
// dropped too and the viewer asks again, so without a budget they multiply: 5-21 Mbps were offered
// to a 2 Mbps link in the M6 test. This interceptor sits inside Pion's NACK responder (registered
// before it) and lets resends use at most rtxShare of the bytes sent fresh in the last second;
// beyond that a lost picture is repaired by a keyframe on the viewer's picture-loss report.
const (
	rtxShare = 0.3
	rtxBurst = 4 << 10 // allowed even before a second of history exists
)

type rtxFactory struct{}

func (rtxFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	return &rtxLimiter{}, nil
}

type rtxLimiter struct{ interceptor.NoOp }

type rtxWindow struct {
	mu          sync.Mutex
	started     bool
	highest     uint16
	start       time.Time
	fresh, prev int // bytes sent fresh in the current and previous second
	resent      int // bytes resent in the current second
}

// resend reports whether a packet repeats one already sent, and whether its budget allows it.
func (w *rtxWindow) allow(seq uint16, n int, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if now.Sub(w.start) >= time.Second {
		w.prev, w.fresh, w.resent, w.start = w.fresh, 0, 0, now
	}
	if !w.started || int16(seq-w.highest) > 0 {
		w.started, w.highest = true, seq
		w.fresh += n
		return true
	}
	budget := int(rtxShare*float64(max(w.fresh, w.prev))) + rtxBurst
	if w.resent+n > budget {
		return false
	}
	w.resent += n
	return true
}

func (*rtxLimiter) BindLocalStream(_ *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	w := &rtxWindow{}
	return interceptor.RTPWriterFunc(func(h *rtp.Header, payload []byte, a interceptor.Attributes) (int, error) {
		if !w.allow(h.SequenceNumber, len(payload)+h.MarshalSize(), time.Now()) {
			return len(payload), nil // over the retransmission budget: drop
		}
		return writer.Write(h, payload, a)
	})
}
