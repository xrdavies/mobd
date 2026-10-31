package stream

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/xrdavies/mobd/internal/desktop"
)

// fakeAudio sends a packet every 20 ms until closed.
type fakeAudio struct {
	ch     chan desktop.AudioPacket
	closed chan struct{}
	once   sync.Once
}

func newFakeAudio() *fakeAudio {
	a := &fakeAudio{ch: make(chan desktop.AudioPacket, 4), closed: make(chan struct{})}
	go func() {
		defer close(a.ch)
		for i := 1; ; i++ {
			select {
			case <-a.closed:
				return
			case <-time.After(desktop.AudioFrame):
			}
			select {
			case a.ch <- desktop.AudioPacket{Data: []byte{0xfc, byte(i)}, PTS: time.Duration(i) * desktop.AudioFrame}:
			case <-a.closed:
				return
			}
		}
	}()
	return a
}

func (a *fakeAudio) Packets() <-chan desktop.AudioPacket { return a.ch }
func (a *fakeAudio) Err() error                          { return nil }
func (a *fakeAudio) Close()                              { a.once.Do(func() { close(a.closed) }) }

// soundViewer offers video and audio and opens the control channel, like the apps with sound.
type soundViewer struct {
	pc      *webrtc.PeerConnection
	control *webrtc.DataChannel
	audio   atomic.Int32
	mu      sync.Mutex
	states  []string
}

func (v *soundViewer) state() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.states) == 0 {
		return ""
	}
	return v.states[len(v.states)-1]
}

func dialSound(t *testing.T, m *Manager, req Request, withAudio bool) (*soundViewer, Answer) {
	t.Helper()
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	pc, err := viewerAPI(t, se).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	v := &soundViewer{pc: pc}
	recv := webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, recv); err != nil {
		t.Fatal(err)
	}
	if withAudio {
		if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, recv); err != nil {
			t.Fatal(err)
		}
	}
	v.control, _ = pc.CreateDataChannel("control", nil)
	v.control.OnMessage(func(msg webrtc.DataChannelMessage) {
		var m map[string]string
		if json.Unmarshal(msg.Data, &m) == nil && m["t"] == "audio" {
			v.mu.Lock()
			v.states = append(v.states, m["state"])
			v.mu.Unlock()
		}
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			if _, _, err := track.ReadRTP(); err != nil {
				return
			}
			if track.Kind() == webrtc.RTPCodecTypeAudio {
				v.audio.Add(1)
			}
		}
	})
	offer, _ := pc.CreateOffer(nil)
	gathered := webrtc.GatheringCompletePromise(pc)
	_ = pc.SetLocalDescription(offer)
	<-gathered
	req.Offer = pc.LocalDescription().SDP
	a, err := m.Open("desk", req)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: a.Answer}); err != nil {
		t.Fatal(err)
	}
	return v, a
}

func (v *soundViewer) send(t *testing.T, on bool) {
	t.Helper()
	out, _ := json.Marshal(map[string]any{"t": "audio", "on": on})
	if err := v.control.SendText(string(out)); err != nil {
		t.Fatal(err)
	}
}

func TestSoundIsOfferedInStereoAndTurnsOnAndOff(t *testing.T) {
	var started atomic.Int32
	var live atomic.Pointer[fakeAudio]
	h := harnessWith(t, func(c *Config) {
		c.Audio = func(context.Context) (desktop.AudioStream, error) {
			started.Add(1)
			a := newFakeAudio()
			live.Store(a)
			return a, nil
		}
	})
	v, a := dialSound(t, h.m, Request{Audio: true}, true)
	if a.Audio == nil || a.Audio.Status != "ready" || a.Audio.Codec != "opus" || a.Audio.Channels != 2 || a.Audio.SampleRate != 48000 {
		t.Fatalf("audio result %+v", a.Audio)
	}
	if !strings.Contains(a.Answer, "opus/48000/2") || !strings.Contains(a.Answer, "stereo=1") {
		t.Fatalf("answer lacks stereo Opus:\n%s", a.Answer)
	}
	waitFor(t, "control channel", func() bool { return v.control.ReadyState() == webrtc.DataChannelStateOpen })
	time.Sleep(200 * time.Millisecond)
	if started.Load() != 0 || v.audio.Load() != 0 {
		t.Fatal("sound started before the viewer turned it on")
	}
	v.send(t, true)
	waitFor(t, "sound on", func() bool { return v.state() == "on" })
	waitFor(t, "audio packets", func() bool { return v.audio.Load() > 10 })
	v.send(t, false)
	waitFor(t, "sound off", func() bool { return v.state() == "off" })
	select {
	case <-live.Load().closed:
	case <-time.After(time.Second):
		t.Fatal("capture still running after sound off")
	}
	// On again, then closing the session stops the capture too.
	v.send(t, true)
	waitFor(t, "sound on again", func() bool { return started.Load() == 2 && v.state() == "on" })
	h.m.Close("desk", a.Session)
	select {
	case <-live.Load().closed:
	case <-time.After(time.Second):
		t.Fatal("capture still running after the session closed")
	}
}

func TestSoundUnavailableKeepsVideo(t *testing.T) {
	h := newHarness(t) // no audio capture configured
	_, a := dialSound(t, h.m, Request{Audio: true}, true)
	if a.Audio == nil || a.Audio.Status != "unavailable" || a.Audio.Reason == "" {
		t.Fatalf("audio result %+v", a.Audio)
	}
	if strings.Contains(a.Answer, "msid:mobd sound") {
		t.Fatalf("answer has an audio track:\n%s", a.Answer)
	}
	h2 := harnessWith(t, func(c *Config) {
		c.Audio = func(context.Context) (desktop.AudioStream, error) { return newFakeAudio(), nil }
	})
	_, b := dialSound(t, h2.m, Request{Audio: true}, false)
	if b.Audio == nil || b.Audio.Status != "unavailable" {
		t.Fatalf("offer without audio: %+v", b.Audio)
	}
	_, c := dialSound(t, h2.m, Request{}, true)
	if c.Audio != nil {
		t.Fatalf("audio not asked for, got %+v", c.Audio)
	}
	if ws, err := h2.m.Open("ws-client", Request{Audio: true}); err != nil || ws.Audio == nil || ws.Audio.Status != "unavailable" {
		t.Fatalf("WebSocket-only: %+v %v", ws.Audio, err)
	}
}

func TestStereoOpusMarksOnlyOpus(t *testing.T) {
	in := "m=audio 9 UDP/TLS/RTP/SAVPF 111 9\r\na=rtpmap:111 opus/48000/2\r\na=fmtp:111 minptime=10;useinbandfec=1\r\na=rtpmap:9 G722/8000\r\na=fmtp:9 x=1\r\n"
	out := stereoOpus(in)
	if !strings.Contains(out, "a=fmtp:111 minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1\r\n") || !strings.Contains(out, "a=fmtp:9 x=1\r\n") {
		t.Fatalf("got %q", out)
	}
	if stereoOpus(out) != out {
		t.Fatal("not idempotent")
	}
}
