package stream

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pion/webrtc/v4"

	"github.com/xrdavies/mobd/internal/desktop"
)

type fakeInput struct {
	mu       sync.Mutex
	events   []string
	released bool
	cursorX  float64
}

func (f *fakeInput) add(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, s)
	return nil
}
func (f *fakeInput) Move(x, y float64) error { return f.add(fmt.Sprintf("move %.2f,%.2f", x, y)) }
func (f *fakeInput) Button(x, y float64, b desktop.Button, down bool) error {
	return f.add(fmt.Sprintf("button %d %t %.2f,%.2f", b, down, x, y))
}
func (f *fakeInput) Key(code string, down bool) error {
	if code == "Bogus" {
		return fmt.Errorf("unknown key code %q", code)
	}
	return f.add(fmt.Sprintf("key %s %t", code, down))
}
func (f *fakeInput) Text(text string) error { return f.add("text " + text) }
func (f *fakeInput) Scroll(x, y, dx, dy float64) error {
	return f.add(fmt.Sprintf("scroll %.0f %.0f", dx, dy))
}
func (f *fakeInput) Cursor() (float64, float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cursorX += 0.01 // moving, so updates keep coming
	return f.cursorX, 0.5, nil
}
func (f *fakeInput) Release() { f.mu.Lock(); f.released = true; f.mu.Unlock() }

func (f *fakeInput) snapshot() ([]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...), f.released
}

func TestDataChannelsDriveInputAndReportCursor(t *testing.T) {
	h := newHarness(t)
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	_, _ = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	ordered, zero := false, uint16(0)
	input, _ := pc.CreateDataChannel("input", nil)
	pointer, _ := pc.CreateDataChannel("pointer", &webrtc.DataChannelInit{Ordered: &ordered, MaxRetransmits: &zero})
	control, _ := pc.CreateDataChannel("control", nil)
	var mu sync.Mutex
	var fromServer []map[string]any
	control.OnMessage(func(msg webrtc.DataChannelMessage) {
		var v map[string]any
		_ = json.Unmarshal(msg.Data, &v)
		mu.Lock()
		fromServer = append(fromServer, v)
		mu.Unlock()
	})
	offer, _ := pc.CreateOffer(nil)
	gathered := webrtc.GatheringCompletePromise(pc)
	_ = pc.SetLocalDescription(offer)
	<-gathered
	a, err := h.m.Open("desk", Request{Offer: pc.LocalDescription().SDP})
	if err != nil {
		t.Fatal(err)
	}
	_ = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: a.Answer})
	opened := func(dc *webrtc.DataChannel) func() bool {
		return func() bool { return dc.ReadyState() == webrtc.DataChannelStateOpen }
	}
	waitFor(t, "channels open", func() bool { return opened(input)() && opened(pointer)() && opened(control)() })
	waitFor(t, "cursor updates", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(fromServer) > 2 && fromServer[0]["t"] == "cursor"
	})

	send := func(dc *webrtc.DataChannel, s string) { _ = dc.SendText(s) }
	send(pointer, `{"t":"move","x":0.25,"y":0.5}`)
	waitFor(t, "move", func() bool { e, _ := h.input.snapshot(); return len(e) == 1 })
	for _, m := range []string{
		`{"t":"button","button":"left","down":true,"x":0.25,"y":0.5}`,
		`{"t":"button","button":"left","down":false,"x":0.25,"y":0.5}`,
		`{"t":"key","code":"MetaLeft","down":true}`,
		`{"t":"key","code":"KeyC","down":true}`,
		`{"t":"text","text":"héllo"}`,
		`{"t":"scroll","x":0.5,"y":0.5,"dx":0,"dy":120}`,
		`{"t":"button","button":"thumb","down":true}`,
		`{"t":"key","code":"Bogus","down":true}`,
		`not json`,
	} {
		send(input, m)
	}
	before := h.last().keyframes.Load()
	send(control, `{"t":"keyframe"}`)
	waitFor(t, "input events", func() bool { e, _ := h.input.snapshot(); return len(e) == 7 })
	events, _ := h.input.snapshot()
	want := "move 0.25,0.50|button 0 true 0.25,0.50|button 0 false 0.25,0.50|key MetaLeft true|key KeyC true|text héllo|scroll 0 120"
	if got := strings.Join(events, "|"); got != want {
		t.Fatalf("events %s", got)
	}
	waitFor(t, "keyframe request", func() bool { return h.last().keyframes.Load() > before })
	waitFor(t, "error reports", func() bool {
		mu.Lock()
		defer mu.Unlock()
		about := map[string]bool{}
		for _, v := range fromServer {
			if v["t"] == "error" {
				about[v["about"].(string)] = true
			}
		}
		return about["button"] && about["key"] && about["json"]
	})
	h.m.Close("desk", a.Session)
	if _, released := h.input.snapshot(); !released {
		t.Fatal("held input not released when the session ended")
	}
}
