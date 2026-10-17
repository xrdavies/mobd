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
//
// Resends come in two forms: on the RTX stream (its own SSRC and sequence numbers) when the viewer
// negotiated RTX, as browsers do, or repeating the video's own sequence numbers otherwise. The two
// sequence spaces must not be compared: when RTX numbering started above the video's, fresh video
// packets were taken for resends and dropped, and rate control collapsed on the "loss".
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

// allow reports whether a packet may be sent: always when fresh, within the budget when a resend.
// rtx marks packets on the RTX stream; seq is then from the RTX sequence space and not compared.
func (w *rtxWindow) allow(seq uint16, rtx bool, n int, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if now.Sub(w.start) >= time.Second {
		w.prev, w.fresh, w.resent, w.start = w.fresh, 0, 0, now
	}
	if !rtx && (!w.started || int16(seq-w.highest) > 0) {
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

func (*rtxLimiter) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	w := &rtxWindow{}
	rtxSSRC := info.SSRCRetransmission
	return interceptor.RTPWriterFunc(func(h *rtp.Header, payload []byte, a interceptor.Attributes) (int, error) {
		rtx := rtxSSRC != 0 && h.SSRC == rtxSSRC
		if !w.allow(h.SequenceNumber, rtx, len(payload)+h.MarshalSize(), time.Now()) {
			return len(payload), nil // over the retransmission budget: drop
		}
		return writer.Write(h, payload, a)
	})
}
