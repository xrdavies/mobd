package stream

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"

	"github.com/xrdavies/mobd/internal/desktop"
)

func TestWebSocketFlowControl(t *testing.T) {
	f := &wsFlow{kbps: func() int { return 1000 }} // 125 KB/s: 62 KB before an RTT is known
	now := time.Now()
	unit := func(ms int, n int, key bool) desktop.AccessUnit {
		return desktop.AccessUnit{PTS: time.Duration(ms) * time.Millisecond, Data: make([]byte, n), Key: key}
	}
	if send, _ := f.admit(unit(0, 200<<10, true), now); !send {
		t.Fatal("a unit larger than the budget must pass when nothing is in flight")
	}
	if send, _ := f.admit(unit(33, 1000, false), now); send {
		t.Fatal("sent beyond the budget")
	}
	f.ack(0, now.Add(80*time.Millisecond))
	if l := f.limit(); l != 37500 { // 80 ms RTT: the 300 ms minimum window at 125 KB/s
		t.Fatalf("budget %d", l)
	}
	if send, ask := f.admit(unit(66, 1000, false), now); send || !ask {
		t.Fatalf("after a drop: send=%t ask=%t, want a keyframe request", send, ask)
	}
	if send, ask := f.admit(unit(99, 1000, false), now); send || ask {
		t.Fatal("keyframe asked again within 500 ms, or a delta sent before the keyframe")
	}
	if send, _ := f.admit(unit(132, 5000, true), now); !send {
		t.Fatal("keyframe not sent after the backlog cleared")
	}
	fb := f.feedback(now.Add(time.Second))
	if fb.rtt != 80*time.Millisecond || fb.loss < 0.59 || fb.loss > 0.61 { // 3 of 5 dropped
		t.Fatalf("feedback %+v", fb)
	}
	if fb := f.feedback(now.Add(1500 * time.Millisecond)); fb.rtt != 1500*time.Millisecond || fb.loss != 0 {
		t.Fatalf("unacknowledged unit should read as delay: %+v", fb)
	}
	f.ack(999, now)     // unknown: ignored
	f.ack(132_000, now) // microseconds, as in the header
	if f.bytes != 0 || len(f.queue) != 0 {
		t.Fatalf("in flight %d bytes, %d units", f.bytes, len(f.queue))
	}
}

// wsServer serves GET /?session=ID for client "desk", like the API does after authentication.
func wsServer(t *testing.T, m *Manager) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch err := m.ServeWS("desk", r.URL.Query().Get("session"), w, r); {
		case errors.Is(err, ErrNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, ErrConflict):
			http.Error(w, err.Error(), http.StatusConflict)
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dialWS(t *testing.T, url string) (*websocket.Conn, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		if resp == nil {
			t.Fatal(err)
		}
		return nil, resp.StatusCode
	}
	c.SetReadLimit(8 << 20) // keyframes exceed the 32 KB default
	t.Cleanup(func() { c.CloseNow() })
	return c, http.StatusSwitchingProtocols
}

// readVideo waits for a video message; ack acknowledges it like a client.
func readVideo(t *testing.T, c *websocket.Conn, keyOnly bool, texts *[]map[string]any) (pts int64, key bool, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ == websocket.MessageText {
			var v map[string]any
			_ = json.Unmarshal(data, &v)
			*texts = append(*texts, v)
			continue
		}
		if len(data) < wsHeader || data[0] != wsVideo {
			t.Fatalf("bad video message % x", data[:min(len(data), 12)])
		}
		pts, key = int64(binary.BigEndian.Uint64(data[1:9])), data[9]&wsKeyFlag != 0
		ack, _ := json.Marshal(map[string]any{"t": "ack", "pts": pts})
		_ = c.Write(ctx, websocket.MessageText, ack)
		if key || !keyOnly {
			return pts, key, len(data) - wsHeader
		}
	}
}

func TestWebSocketCarriesTheSession(t *testing.T) {
	h := newHarness(t)
	a, err := h.m.Open("desk", Request{Quality: "auto"}) // no offer: WebSocket only
	if err != nil || a.Answer != "" {
		t.Fatalf("open: %v %q", err, a.Answer)
	}
	url := wsServer(t, h.m)
	if _, code := dialWS(t, url+"/?session=nope"); code != http.StatusNotFound {
		t.Fatalf("unknown session: %d", code)
	}
	start := time.Now()
	c, _ := dialWS(t, url+"/?session="+a.Session)
	var texts []map[string]any
	if _, key, _ := readVideo(t, c, true, &texts); !key {
		t.Fatal("no keyframe")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("first picture after %s", took)
	}
	if _, code := dialWS(t, url+"/?session="+a.Session); code != http.StatusConflict {
		t.Fatalf("second transport: %d", code)
	}
	ctx := context.Background()
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"t":"move","x":0.25,"y":0.5}`))
	waitFor(t, "input over the WebSocket", func() bool { e, _ := h.input.snapshot(); return len(e) == 1 })
	deadline := time.Now().Add(4 * time.Second)
	kinds := map[any]bool{}
	for time.Now().Before(deadline) && !(kinds["cursor"] && kinds["stats"]) {
		readVideo(t, c, false, &texts)
		for _, v := range texts {
			kinds[v["t"]] = true
		}
	}
	if !kinds["cursor"] || !kinds["stats"] {
		t.Fatalf("control messages %v", kinds)
	}
	c.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "session to end with the socket", func() bool {
		_, released := h.input.snapshot()
		return h.m.Count() == 0 && released && h.awake.Load() == 0
	})
}

func TestWebSocketWinsWhenWebRTCCannotConnect(t *testing.T) {
	h := newHarness(t)
	// A real offer whose peer never applies the answer, as when UDP cannot pass.
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	_, _ = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	offer, _ := pc.CreateOffer(nil)
	_ = pc.SetLocalDescription(offer)
	a, err := h.m.Open("desk", Request{Offer: offer.SDP})
	if err != nil || a.Answer == "" {
		t.Fatalf("open: %v", err)
	}
	c, code := dialWS(t, wsServer(t, h.m)+"/?session="+a.Session)
	if c == nil {
		t.Fatalf("WebSocket refused: %d", code)
	}
	var texts []map[string]any
	readVideo(t, c, true, &texts)
	readVideo(t, c, false, &texts)
	if h.m.Count() != 1 {
		t.Fatal("closing the losing peer connection ended the session")
	}
}

func TestWebRTCWinsTheRace(t *testing.T) {
	h := newHarness(t)
	v, a := connect(t, h.m, "desk", Request{})
	waitFor(t, "RTP packets", func() bool { return v.packets.Load() > 5 })
	if _, code := dialWS(t, wsServer(t, h.m)+"/?session="+a.Session); code != http.StatusConflict {
		t.Fatalf("WebSocket after WebRTC connected: %d", code)
	}
}

// slowProxy forwards TCP to target, limiting the server-to-client direction to kbps, like a slow
// downlink with buffers on the way.
func slowProxy(t *testing.T, target string, kbps int) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", target)
			if err != nil {
				client.Close()
				continue
			}
			go func() { _, _ = io.Copy(server, client); server.Close() }()
			go func() {
				defer client.Close()
				buf := make([]byte, 1500)
				next := time.Now()
				for {
					n, err := server.Read(buf)
					if err != nil {
						return
					}
					if now := time.Now(); next.Before(now) {
						next = now
					}
					time.Sleep(time.Until(next))
					next = next.Add(time.Duration(n*8) * time.Second / time.Duration(kbps*1000))
					if _, err := client.Write(buf[:n]); err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// On an 800 kbps downlink the fallback must keep the picture moving without queueing seconds of
// video: the encoder backs off below the link and the delay stays bounded.
func TestWebSocketOnASlowLink(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 20 s")
	}
	h := newHarness(t)
	a, err := h.m.Open("desk", Request{Quality: "high"}) // starts at 3000 kbps, far above the link
	if err != nil {
		t.Fatal(err)
	}
	url := wsServer(t, h.m)
	proxy := slowProxy(t, strings.TrimPrefix(url, "ws://"), 800)
	c, _ := dialWS(t, "ws://"+proxy+"/?session="+a.Session)
	var texts []map[string]any
	var frames, bytes int
	var rtts []float64
	start := time.Now()
	for time.Since(start) < 18*time.Second {
		_, _, n := readVideo(t, c, false, &texts)
		if time.Since(start) > 10*time.Second {
			frames++
			bytes += n
		}
		for _, v := range texts {
			if v["t"] == "stats" && time.Since(start) > 10*time.Second {
				rtts = append(rtts, v["rttMs"].(float64))
			}
		}
		texts = texts[:0]
	}
	kbps := bytes * 8 / 8000
	t.Logf("last 8 s: %d frames, %d kbps received, target %d kbps, rtt ms %v", frames, kbps, h.last().kbps.Load(), rtts)
	if frames < 40 || kbps < 200 || kbps > 850 {
		t.Fatalf("picture stalled or exceeded the link: %d frames, %d kbps", frames, kbps)
	}
	for _, r := range rtts {
		if r > 1500 {
			t.Fatalf("delay built up: rtt %v ms", rtts)
		}
	}
}
