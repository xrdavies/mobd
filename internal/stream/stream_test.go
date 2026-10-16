package stream

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"github.com/xrdavies/mobd/internal/desktop"
)

// fakeStream emits a small access unit every 20 ms; keyframes on request.
type fakeStream struct {
	opts      desktop.StreamOptions
	units     chan desktop.AccessUnit
	keyframes atomic.Int32
	closeOnce sync.Once
	closed    chan struct{}
}

func newFake(opts desktop.StreamOptions) *fakeStream {
	f := &fakeStream{opts: opts, units: make(chan desktop.AccessUnit, 4), closed: make(chan struct{})}
	go func() {
		defer close(f.units)
		pts := time.Duration(0)
		for {
			select {
			case <-f.closed:
				return
			case <-time.After(20 * time.Millisecond):
			}
			pts += 20 * time.Millisecond
			// AUD + a non-IDR slice; the payload content does not matter to the packetizer.
			au := desktop.AccessUnit{Data: []byte{0, 0, 0, 1, 9, 0xf0, 0, 0, 0, 1, 0x41, 0x9a, 1, 2, 3}, PTS: pts}
			select {
			case f.units <- au:
			case <-f.closed:
				return
			}
		}
	}()
	return f
}

func (f *fakeStream) Units() <-chan desktop.AccessUnit { return f.units }
func (f *fakeStream) SetBitrate(int)                   {}
func (f *fakeStream) SetSize(int, int)                 {}
func (f *fakeStream) Keyframe()                        { f.keyframes.Add(1) }
func (f *fakeStream) Err() error                       { return nil }
func (f *fakeStream) Close()                           { f.closeOnce.Do(func() { close(f.closed) }) }

type harness struct {
	m       *Manager
	mu      sync.Mutex
	streams []*fakeStream
	awake   atomic.Int32
	cancels map[string]context.CancelFunc
}

func newHarness(t *testing.T) *harness {
	h := &harness{cancels: map[string]context.CancelFunc{}}
	m, err := NewManager(Config{
		Loopback: true,
		Display: func() (desktop.Display, error) {
			return desktop.Display{Width: 1920, Height: 1080, Scale: 2, Main: true}, nil
		},
		Start: func(_ context.Context, opts desktop.StreamOptions) (desktop.Stream, error) {
			f := newFake(opts)
			h.mu.Lock()
			h.streams = append(h.streams, f)
			h.mu.Unlock()
			return f, nil
		},
		KeepAwake: func(string) (func(), error) {
			h.awake.Add(1)
			var once sync.Once
			return func() { once.Do(func() { h.awake.Add(-1) }) }, nil
		},
		// Stands in for pairing: revoke(client) cancels every context attached for it.
		Attach: func(parent context.Context, client string) (context.Context, func(), error) {
			if client == "revoked" {
				return nil, nil, errors.New("pairing required")
			}
			ctx, cancel := context.WithCancel(parent)
			h.mu.Lock()
			h.cancels[client] = cancel
			h.mu.Unlock()
			return ctx, cancel, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.m = m
	t.Cleanup(m.Shutdown)
	return h
}

func (h *harness) last() *fakeStream {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.streams[len(h.streams)-1]
}

// viewer is a receive-only Pion peer, like the apps will be.
type viewer struct {
	pc      *webrtc.PeerConnection
	packets atomic.Int32
	track   chan *webrtc.TrackRemote
	recv    *webrtc.RTPReceiver
}

func connect(t *testing.T, m *Manager, client string, req Request) (*viewer, Answer) {
	t.Helper()
	return dial(t, m, client, req, true)
}

// connectReal leaves reading the track to the caller.
func connectReal(t *testing.T, m *Manager) (*viewer, Answer) {
	t.Helper()
	return dial(t, m, "desk", Request{}, false)
}

func dial(t *testing.T, m *Manager, client string, req Request, count bool) (*viewer, Answer) {
	t.Helper()
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	v := &viewer{pc: pc, track: make(chan *webrtc.TrackRemote, 1)}
	tr, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	if err != nil {
		t.Fatal(err)
	}
	v.recv = tr.Receiver()
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		v.track <- track
		if !count {
			return
		}
		for {
			if _, _, err := track.ReadRTP(); err != nil {
				return
			}
			v.packets.Add(1)
		}
	})
	offer, _ := pc.CreateOffer(nil)
	gathered := webrtc.GatheringCompletePromise(pc)
	_ = pc.SetLocalDescription(offer)
	<-gathered
	req.Offer = pc.LocalDescription().SDP
	a, err := m.Open(client, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: a.Answer}); err != nil {
		t.Fatal(err)
	}
	return v, a
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFitKeepsAspectWithinViewDisplayAndProfile(t *testing.T) {
	for _, tc := range []struct{ dw, dh, mw, mh, w, h int }{
		{3840, 2160, 0, 0, 1920, 1080},     // profile cap
		{3840, 2160, 720, 0, 720, 406},     // side panel
		{3840, 2160, 2000, 400, 712, 400},  // short view
		{1280, 800, 1600, 1000, 1280, 800}, // never upscale
	} {
		if w, h := fit(tc.dw, tc.dh, tc.mw, tc.mh, profiles["high"]); w != tc.w || h != tc.h {
			t.Fatalf("%+v: got %dx%d", tc, w, h)
		}
	}
	if w, h := fit(3840, 2160, 0, 0, profiles["low"]); w != 1280 || h != 720 {
		t.Fatalf("low: %dx%d", w, h)
	}
}

func TestSessionStreamsVideoAndAnswersPictureLoss(t *testing.T) {
	h := newHarness(t)
	v, a := connect(t, h.m, "desk", Request{MaxWidth: 1280, MaxHeight: 800, Quality: "high"})
	if a.Width != 1280 || a.Height != 720 || h.last().opts.Width != 1280 || h.last().opts.FPS != 30 {
		t.Fatalf("answer %+v opts %+v", a, h.last().opts)
	}
	waitFor(t, "RTP packets", func() bool { return v.packets.Load() > 10 })
	// Connecting asked for a keyframe; a picture-loss report asks for another.
	waitFor(t, "keyframe on connect", func() bool { return h.last().keyframes.Load() >= 1 })
	track := <-v.track
	before := h.last().keyframes.Load()
	if err := v.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "keyframe on PLI", func() bool { return h.last().keyframes.Load() > before })
	if h.awake.Load() != 1 || h.m.Count() != 1 {
		t.Fatalf("awake %d sessions %d", h.awake.Load(), h.m.Count())
	}
	if !h.m.Close("desk", a.Session) || h.m.Close("desk", a.Session) {
		t.Fatal("Close should succeed once")
	}
	waitFor(t, "capture stopped and display released", func() bool {
		select {
		case <-h.last().closed:
			return h.awake.Load() == 0 && h.m.Count() == 0
		default:
			return false
		}
	})
}

func TestNewSessionReplacesOldAndRevokeEndsIt(t *testing.T) {
	h := newHarness(t)
	_, first := connect(t, h.m, "desk", Request{})
	firstStream := h.last()
	_, second := connect(t, h.m, "desk", Request{Quality: "low"})
	if first.Session == second.Session || h.m.Count() != 1 || h.last().opts.Width != 1280 || h.last().opts.FPS != 15 {
		t.Fatalf("replace: %d sessions, opts %+v", h.m.Count(), h.last().opts)
	}
	select {
	case <-firstStream.closed:
	case <-time.After(time.Second):
		t.Fatal("old capture still running")
	}
	if h.m.Close("other", second.Session) {
		t.Fatal("another client closed the session")
	}
	h.mu.Lock()
	revoke := h.cancels["desk"]
	h.mu.Unlock()
	revoke()
	waitFor(t, "revoke to end the session", func() bool { return h.m.Count() == 0 && h.awake.Load() == 0 })
	if _, err := h.m.Open("revoked", Request{Offer: "v=0"}); err == nil {
		t.Fatal("revoked client opened a stream")
	}
	if _, err := h.m.Open("desk", Request{Offer: "v=0", Quality: "ultra"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("bad quality: %v", err)
	}
	h.m.Shutdown()
	if _, err := h.m.Open("desk", Request{Offer: "v=0"}); err == nil {
		t.Fatal("open after shutdown")
	}
}
