// Package stream serves the live desktop over WebRTC: one Pion session per paired client, an
// H.264 track fed by desktop.StartStream, keyframes on request, and teardown when the client is
// revoked. See docs/desktop-streaming.md.
package stream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/xrdavies/mobd/internal/desktop"
)

// ErrBadRequest marks requests the client must fix (HTTP 400).
var ErrBadRequest = errors.New("invalid stream request")

// connectTimeout ends a session whose peer never connects, so its capture does not run forever.
const connectTimeout = 30 * time.Second

// Request opens a session. MaxWidth and MaxHeight are the client's view in pixels; the stream is
// never larger than the view, the display or the quality profile.
type Request struct {
	Offer     string `json:"offer"`
	MaxWidth  int    `json:"maxWidth"`
	MaxHeight int    `json:"maxHeight"`
	Quality   string `json:"quality"`
}

// Answer is returned to the client; Width and Height are the encoded size.
type Answer struct {
	Session string          `json:"session"`
	Answer  string          `json:"answer"`
	Width   int             `json:"width"`
	Height  int             `json:"height"`
	Display desktop.Display `json:"display"`
}

// profile bounds a quality setting. Auto follows High until adaptation lands (M6).
type profile struct{ width, height, fps, kbps int }

var profiles = map[string]profile{
	"high": {1920, 1080, 30, 4000},
	"auto": {1920, 1080, 30, 4000},
	"low":  {1280, 720, 15, 1000},
}

// Config connects the manager to the desktop and to pairing; tests substitute fakes.
type Config struct {
	Start     func(context.Context, desktop.StreamOptions) (desktop.Stream, error)
	Display   func() (desktop.Display, error)
	KeepAwake func(reason string) (func(), error)
	// Attach ties a session to its client: the context ends when the client is revoked.
	Attach func(parent context.Context, clientID string) (context.Context, func(), error)
	// NewInput gives a session control of the desktop; nil makes sessions view-only.
	NewInput func() Input
	// Loopback adds 127.0.0.1 candidates; only for tests on one machine.
	Loopback bool
}

// Manager owns the live sessions, at most one per client.
type Manager struct {
	cfg    Config
	api    *webrtc.API
	base   context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	byClient map[string]*session
}

func NewManager(cfg Config) (*Manager, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	reg := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, reg); err != nil {
		return nil, err
	}
	se := webrtc.SettingEngine{}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	se.SetIncludeLoopbackCandidate(cfg.Loopback)
	base, cancel := context.WithCancel(context.Background())
	return &Manager{
		cfg: cfg, base: base, cancel: cancel, byClient: make(map[string]*session),
		api: webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg), webrtc.WithSettingEngine(se)),
	}, nil
}

// fit scales the display's pixel size down to the view and the profile, keeping the aspect ratio.
func fit(dw, dh, maxW, maxH int, p profile) (int, int) {
	scale := math.Min(1, math.Min(float64(p.width)/float64(dw), float64(p.height)/float64(dh)))
	if maxW > 0 {
		scale = math.Min(scale, float64(maxW)/float64(dw))
	}
	if maxH > 0 {
		scale = math.Min(scale, float64(maxH)/float64(dh))
	}
	even := func(v float64) int { return max(16, int(math.Round(v/2))*2) }
	return even(float64(dw) * scale), even(float64(dh) * scale)
}

// Open answers the client's offer and starts its session, replacing any session it already has.
func (m *Manager) Open(clientID string, req Request) (Answer, error) {
	if m.base.Err() != nil {
		return Answer{}, errors.New("stream service stopped")
	}
	if req.Quality == "" {
		req.Quality = "auto"
	}
	p, ok := profiles[req.Quality]
	if !ok || req.Offer == "" || req.MaxWidth < 0 || req.MaxHeight < 0 || req.MaxWidth > 8192 || req.MaxHeight > 8192 {
		return Answer{}, fmt.Errorf("%w: needs an SDP offer, quality auto/high/low and a view of at most 8192 pixels", ErrBadRequest)
	}
	disp, err := m.cfg.Display()
	if err != nil {
		return Answer{}, err
	}
	scale := max(disp.Scale, 1)
	w, h := fit(int(float64(disp.Width)*scale), int(float64(disp.Height)*scale), req.MaxWidth, req.MaxHeight, p)

	ctx, finish, err := m.cfg.Attach(m.base, clientID)
	if err != nil {
		return Answer{}, err
	}
	m.mu.Lock()
	old := m.byClient[clientID]
	m.mu.Unlock()
	if old != nil {
		old.close()
	}
	s := &session{m: m, id: randomID(), client: clientID, finish: finish}
	s.ctx, s.cancel = context.WithCancel(ctx)
	answer, err := s.start(req.Offer, desktop.StreamOptions{Width: w, Height: h, FPS: p.fps, Kbps: p.kbps})
	if err != nil {
		s.close()
		return Answer{}, err
	}
	m.mu.Lock()
	m.byClient[clientID] = s
	m.mu.Unlock()
	log.Printf("stream %s opened for client %.8s at %dx%d (%s)", s.id, clientID, w, h, req.Quality)
	return Answer{Session: s.id, Answer: answer, Width: w, Height: h, Display: disp}, nil
}

// Close ends the client's session with this id; it reports whether there was one.
func (m *Manager) Close(clientID, id string) bool {
	m.mu.Lock()
	s := m.byClient[clientID]
	m.mu.Unlock()
	if s == nil || s.id != id {
		return false
	}
	s.close()
	return true
}

// Count reports the open sessions.
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byClient)
}

// Shutdown ends every session; Open fails afterwards.
func (m *Manager) Shutdown() {
	m.cancel()
	m.mu.Lock()
	all := make([]*session, 0, len(m.byClient))
	for _, s := range m.byClient {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		s.close()
	}
}

type session struct {
	m              *Manager
	id, client     string
	ctx            context.Context
	cancel         context.CancelFunc
	finish         func()
	pc             *webrtc.PeerConnection
	src            desktop.Stream
	release        func()
	once           sync.Once
	ctl            *controller
	connected      chan struct{}
	connectedOnce  sync.Once
	lastKeyRequest time.Time
	keyMu          sync.Mutex
}

func (s *session) start(offer string, opts desktop.StreamOptions) (string, error) {
	// Wake the display first: capture finds no display while it sleeps.
	if release, err := s.m.cfg.KeepAwake("mobd live desktop"); err == nil {
		s.release = release
	} else if !errors.Is(err, desktop.ErrUnsupported) {
		log.Printf("stream %s: %v", s.id, err)
	}
	src, err := s.m.cfg.Start(s.ctx, opts)
	if errors.Is(err, desktop.ErrScreenUnavailable) && s.release != nil {
		// A display that was asleep needs a moment after the wake-up request.
		select {
		case <-time.After(1500 * time.Millisecond):
		case <-s.ctx.Done():
		}
		src, err = s.m.cfg.Start(s.ctx, opts)
	}
	if err != nil {
		return "", err
	}
	s.src = src

	pc, err := s.m.api.NewPeerConnection(webrtc.Configuration{}) // host candidates: LAN and Tailscale
	if err != nil {
		return "", err
	}
	s.pc = pc
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	}, "desktop", "mobd")
	if err != nil {
		return "", err
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		return "", err
	}
	if s.m.cfg.NewInput != nil {
		s.ctl = &controller{s: s, input: s.m.cfg.NewInput()}
		pc.OnDataChannel(s.ctl.attach)
	}
	s.connected = make(chan struct{})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		switch st {
		case webrtc.PeerConnectionStateConnected:
			s.connectedOnce.Do(func() { close(s.connected) })
			s.src.Keyframe()
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			go s.close()
		}
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return "", err
	}
	// No trickle: the answer carries every candidate, so signaling is one request.
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
	}

	go s.readRTCP(sender)
	go s.forward(track, opts.FPS)
	go s.watch()
	return pc.LocalDescription().SDP, nil
}

// forward writes access units to the track until the desktop stream ends.
func (s *session) forward(track *webrtc.TrackLocalStaticSample, fps int) {
	var last time.Duration
	for u := range s.src.Units() {
		d := time.Second / time.Duration(fps)
		if last != 0 && u.PTS > last {
			d = u.PTS - last
		}
		last = u.PTS
		if err := track.WriteSample(media.Sample{Data: u.Data, Duration: d}); err != nil {
			break
		}
	}
	if err := s.src.Err(); err != nil {
		log.Printf("stream %s: %v", s.id, err)
	}
	s.close()
}

// readRTCP answers picture-loss reports with a keyframe, at most twice a second.
func (s *session) readRTCP(sender *webrtc.RTPSender) {
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch p.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				s.keyMu.Lock()
				if time.Since(s.lastKeyRequest) >= 500*time.Millisecond {
					s.lastKeyRequest = time.Now()
					s.src.Keyframe()
				}
				s.keyMu.Unlock()
			}
		}
	}
}

// watch ends the session on revoke, shutdown, or a peer that never connects.
func (s *session) watch() {
	select {
	case <-s.ctx.Done():
	case <-s.connected:
		<-s.ctx.Done()
	case <-time.After(connectTimeout):
		log.Printf("stream %s: peer did not connect within %s", s.id, connectTimeout)
	}
	s.close()
}

func (s *session) close() {
	s.once.Do(func() {
		s.cancel()
		if s.pc != nil {
			_ = s.pc.Close()
		}
		if s.src != nil {
			s.src.Close()
		}
		if s.ctl != nil {
			s.ctl.input.Release() // no key or button stays down after the client leaves
		}
		if s.release != nil {
			s.release()
		}
		s.finish()
		s.m.mu.Lock()
		if s.m.byClient[s.client] == s {
			delete(s.m.byClient, s.client)
		}
		s.m.mu.Unlock()
		log.Printf("stream %s closed", s.id)
	})
}

func randomID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
