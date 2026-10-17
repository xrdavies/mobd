package stream

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/xrdavies/mobd/internal/desktop"
)

// The WebSocket fallback carries a session where UDP cannot pass (Tailscale Funnel, an HTTPS
// gateway, a blocked network). Binary messages from mobd are video:
//
//	byte 0     1 (H.264 access unit, Annex B)
//	bytes 1-8  capture time in microseconds, big endian
//	byte 9     flags: bit 0 keyframe
//	bytes 10-  access unit
//
// Text messages use the data-channel JSON schema in both directions, plus {"t":"ack","pts":N} from
// the client for every video message it receives. TCP never drops, so mobd measures the link from
// the acks: round-trip time, and bytes not yet acknowledged.

const (
	wsVideo    = 1
	wsHeader   = 10
	wsKeyFlag  = 1
	wsMinQueue = 16 << 10 // bytes allowed in flight even at the lowest bitrates
)

// ServeWS connects the client's session over a WebSocket. It returns ErrNotFound or ErrConflict
// before upgrading; otherwise it blocks until the session ends. The caller checks the Origin.
func (m *Manager) ServeWS(clientID, id string, w http.ResponseWriter, r *http.Request) error {
	m.mu.Lock()
	s := m.byClient[clientID]
	m.mu.Unlock()
	if s == nil || s.id != id {
		return ErrNotFound
	}
	if s.chosen() != "" {
		return ErrConflict
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return nil // Accept wrote the HTTP error
	}
	defer ws.CloseNow()
	ws.SetReadLimit(maxMessage)
	flow := &wsFlow{kbps: func() int { return int(s.targetKbps.Load()) }}
	sink := func(u desktop.AccessUnit) error {
		send, askKey := flow.admit(u, time.Now())
		if askKey {
			s.src.Keyframe()
		}
		if !send {
			return nil
		}
		msg := make([]byte, wsHeader+len(u.Data))
		msg[0] = wsVideo
		binary.BigEndian.PutUint64(msg[1:9], uint64(u.PTS.Microseconds()))
		if u.Key {
			msg[9] = wsKeyFlag
		}
		copy(msg[wsHeader:], u.Data)
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		defer cancel()
		return ws.Write(ctx, websocket.MessageBinary, msg)
	}
	if !s.choose("ws", sink, flow.feedback) {
		_ = ws.Close(websocket.StatusPolicyViolation, "session already connected over WebRTC")
		return nil
	}
	if s.ctl != nil {
		s.ctl.setControl(wsText{ws: ws, ctx: s.ctx})
	}
	go func() {
		<-s.ctx.Done()
		_ = ws.Close(websocket.StatusNormalClosure, "session ended")
	}()
	for {
		typ, data, err := ws.Read(s.ctx)
		if err != nil {
			break
		}
		if typ != websocket.MessageText {
			continue
		}
		var m struct {
			T   string `json:"t"`
			PTS int64  `json:"pts"`
		}
		if json.Unmarshal(data, &m) == nil && m.T == "ack" {
			flow.ack(m.PTS, time.Now())
		} else if s.ctl != nil {
			s.ctl.handle(data)
		}
	}
	s.close()
	return nil
}

// wsText sends control messages on the WebSocket; Write is safe alongside video writes.
type wsText struct {
	ws  *websocket.Conn
	ctx context.Context
}

func (t wsText) SendText(msg string) error {
	ctx, cancel := context.WithTimeout(t.ctx, 5*time.Second)
	defer cancel()
	return t.ws.Write(ctx, websocket.MessageText, []byte(msg))
}

type inflight struct {
	pts   int64 // microseconds, as in the header
	bytes int
	at    time.Time
}

// wsFlow keeps about 1.5 round trips of video in flight (0.3-2 s at the target bitrate): enough to
// fill the link, not enough to queue seconds of stale picture in TCP buffers. Beyond that it drops
// frames until the backlog halves and a keyframe arrives; dropped frames count as loss and
// unacknowledged time as delay, so the rate controller backs off the same way it does on WebRTC.
type wsFlow struct {
	kbps func() int

	mu         sync.Mutex
	queue      []inflight
	bytes      int
	rtts       []time.Duration
	sent       int
	dropped    int
	needKey    bool
	lastKeyAsk time.Time
	minRTT     time.Duration
}

// limit is the in-flight budget in bytes; the caller holds f.mu.
func (f *wsFlow) limit() int {
	window := 500 * time.Millisecond
	if f.minRTT > 0 {
		window = min(max(f.minRTT*3/2, 300*time.Millisecond), 2*time.Second)
	}
	return max(int(float64(f.kbps())*1000/8*window.Seconds()), wsMinQueue)
}

// admit decides whether to send a unit now and whether to ask the encoder for a keyframe.
func (f *wsFlow) admit(u desktop.AccessUnit, now time.Time) (send, askKey bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	limit := f.limit()
	switch {
	case f.bytes > 0 && f.bytes+len(u.Data) > limit:
		f.needKey = true
		f.dropped++
		return false, false
	case f.needKey && !u.Key:
		f.dropped++
		if f.bytes <= limit/2 && now.Sub(f.lastKeyAsk) >= 500*time.Millisecond {
			f.lastKeyAsk = now
			return false, true
		}
		return false, false
	}
	f.needKey = false
	f.sent++
	f.queue = append(f.queue, inflight{pts: u.PTS.Microseconds(), bytes: len(u.Data), at: now})
	f.bytes += len(u.Data)
	return true, false
}

// ack retires every unit up to and including the acknowledged one.
func (f *wsFlow) ack(pts int64, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, q := range f.queue {
		if q.pts != pts {
			continue
		}
		rtt := now.Sub(q.at)
		f.rtts = append(f.rtts, rtt)
		if f.minRTT == 0 || rtt < f.minRTT {
			f.minRTT = rtt
		}
		for _, done := range f.queue[:i+1] {
			f.bytes -= done.bytes
		}
		f.queue = f.queue[i+1:]
		return
	}
}

func (f *wsFlow) feedback(now time.Time) feedback {
	f.mu.Lock()
	defer f.mu.Unlock()
	fb := feedback{now: now}
	if f.sent+f.dropped > 0 {
		fb.loss = float64(f.dropped) / float64(f.sent+f.dropped)
	}
	if len(f.rtts) > 0 {
		var sum time.Duration
		for _, r := range f.rtts {
			sum += r
		}
		fb.rtt = sum / time.Duration(len(f.rtts))
	} else if len(f.queue) > 0 {
		fb.rtt = now.Sub(f.queue[0].at) // nothing acknowledged: at least this long
	}
	f.rtts, f.sent, f.dropped = f.rtts[:0], 0, 0
	return fb
}
