// Package stream serves the live desktop: one session per paired client, fed by
// desktop.StartStream, carried over WebRTC (Pion) or a WebSocket where UDP cannot pass, with
// keyframes on request and teardown when the client is revoked. See docs/desktop-streaming.md.
package stream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/xrdavies/mobd/internal/desktop"
)

// rateTrace logs every rate-control decision (MOBD_RATE_TRACE=1), for tuning on real links.
var rateTrace = os.Getenv("MOBD_RATE_TRACE") != ""

var (
	// ErrBadRequest marks requests the client must fix (HTTP 400).
	ErrBadRequest = errors.New("invalid stream request")
	// ErrNotFound: no such session for this client (HTTP 404).
	ErrNotFound = errors.New("stream session not found")
	// ErrConflict: the session is already connected over the other transport (HTTP 409).
	ErrConflict = errors.New("stream session already connected")
)

const (
	// connectTimeout ends a session whose peer never connects, so its capture does not run forever.
	connectTimeout = 30 * time.Second
	// wsStartKbps caps where a WebSocket session starts (540p): it is the path through relays and
	// gateways, and a 720p keyframe at 1.2 Mbps took seconds through Funnel (M7).
	wsStartKbps = 450
)

// Request opens a session. MaxWidth and MaxHeight are the client's view in pixels; the stream is
// never larger than the view, the display or the quality profile. Without an offer the session
// waits for the WebSocket only.
type Request struct {
	Offer     string `json:"offer,omitempty"`
	MaxWidth  int    `json:"maxWidth"`
	MaxHeight int    `json:"maxHeight"`
	Quality   string `json:"quality"`
}

// Answer is returned to the client; Width and Height are the encoded size.
type Answer struct {
	Session string `json:"session"`
	Answer  string `json:"answer,omitempty"`
	// WSTicket opens GET /stream/ws once for clients that cannot send headers (set by the API).
	WSTicket string          `json:"wsTicket,omitempty"`
	Width    int             `json:"width"`
	Height   int             `json:"height"`
	Display  desktop.Display `json:"display"`
}

// profile bounds a quality setting: the largest size and frame rate, the bitrate ceiling and where
// rate control starts. All three adapt to the link; they differ in limits and starting point.
type profile struct{ width, height, fps, kbps, startKbps int }

var profiles = map[string]profile{
	"high": {1920, 1080, 30, 6000, 3000},
	"auto": {1920, 1080, 30, 6000, 1200},
	"low":  {1280, 720, 15, 1000, 600},
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
	// UDP, when set, carries every session's media (tests use it to impair the link).
	UDP net.PacketConn
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
	// Before the defaults, so it sits inside the NACK responder and sees its resends (rtx.go).
	reg.Add(rtxFactory{})
	if err := webrtc.RegisterDefaultInterceptors(m, reg); err != nil {
		return nil, err
	}
	se := webrtc.SettingEngine{}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	se.SetIncludeLoopbackCandidate(cfg.Loopback)
	if cfg.UDP != nil {
		se.SetICEUDPMux(webrtc.NewICEUDPMux(nil, cfg.UDP))
	}
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
	if !ok || req.MaxWidth < 0 || req.MaxHeight < 0 || req.MaxWidth > 8192 || req.MaxHeight > 8192 {
		return Answer{}, fmt.Errorf("%w: needs quality auto/high/low and a view of at most 8192 pixels", ErrBadRequest)
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
	s := &session{m: m, id: randomID(), client: clientID, finish: finish,
		display: [2]int{int(float64(disp.Width) * scale), int(float64(disp.Height) * scale)}, profile: p}
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.rate = newRateController(p, w, h, time.Now())
	t := s.rate.current()
	s.applied = t
	s.targetKbps.Store(int32(t.kbps))
	answer, err := s.start(req.Offer, desktop.StreamOptions{Width: t.width, Height: t.height, FPS: t.fps, Kbps: t.kbps})
	if err != nil {
		s.close()
		return Answer{}, err
	}
	m.mu.Lock()
	m.byClient[clientID] = s
	m.mu.Unlock()
	log.Printf("stream %s opened for client %.8s at %dx%d %d kbps (%s, up to %dx%d)", s.id, clientID, t.width, t.height, t.kbps, req.Quality, w, h)
	return Answer{Session: s.id, Answer: answer, Width: t.width, Height: t.height, Display: disp}, nil
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
	display        [2]int // capture size in pixels, for refitting on resize
	profile        profile
	rateMu         sync.Mutex // rate and applied: the adapt loop, the WebSocket start and resize
	rate           *rateController
	applied        target
	lossMu         sync.Mutex
	lossSum        float64 // receiver-reported fraction lost since the last rate tick
	lossReports    int
	targetKbps     atomic.Int32 // the encoder's current target, read by the WebSocket flow control
	connected      chan struct{}
	connectedOnce  sync.Once
	lastKeyRequest time.Time
	keyMu          sync.Mutex

	mu        sync.Mutex
	transport string                         // "webrtc" or "ws" once one connects
	sink      func(desktop.AccessUnit) error // writes to the connected transport
	feedback  func(now time.Time) feedback   // the connected transport's view of the link
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
	if s.m.cfg.NewInput != nil {
		s.ctl = &controller{s: s, input: s.m.cfg.NewInput()}
	}
	s.connected = make(chan struct{})
	answer := ""
	if offer != "" {
		if answer, err = s.startWebRTC(offer, opts.FPS); err != nil {
			return "", err
		}
	}
	go s.adapt()
	go s.forward()
	go s.watch()
	return answer, nil
}

// choose makes the first transport to connect carry the session; it reports false when the other
// one already does.
func (s *session) choose(transport string, sink func(desktop.AccessUnit) error, fb func(time.Time) feedback) bool {
	s.mu.Lock()
	if s.transport != "" {
		s.mu.Unlock()
		return false
	}
	s.transport, s.sink, s.feedback = transport, sink, fb
	pc := s.pc
	s.mu.Unlock()
	if transport == "ws" {
		s.rateMu.Lock()
		s.apply(s.rate.startAt(wsStartKbps), "WebSocket start")
		s.rateMu.Unlock()
	}
	s.connectedOnce.Do(func() { close(s.connected) })
	if transport == "ws" && pc != nil {
		go pc.Close() // the WebSocket won the race
	}
	s.src.Keyframe() // a picture right away, not at the next change
	log.Printf("stream %s connected over %s", s.id, transport)
	return true
}

func (s *session) chosen() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transport
}

func (s *session) startWebRTC(offer string, fps int) (string, error) {
	pc, err := s.m.api.NewPeerConnection(webrtc.Configuration{}) // host candidates: LAN and Tailscale
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.pc = pc
	s.mu.Unlock()
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
	if s.ctl != nil {
		pc.OnDataChannel(s.ctl.attach)
	}
	var last time.Duration
	sink := func(u desktop.AccessUnit) error {
		d := time.Second / time.Duration(fps)
		if last != 0 && u.PTS > last {
			d = u.PTS - last
		}
		last = u.PTS
		return track.WriteSample(media.Sample{Data: u.Data, Duration: d})
	}
	fb := func(now time.Time) feedback {
		s.lossMu.Lock()
		loss := 0.0
		if s.lossReports > 0 {
			loss = s.lossSum / float64(s.lossReports)
		}
		s.lossSum, s.lossReports = 0, 0
		s.lossMu.Unlock()
		var rtt time.Duration
		for _, st := range pc.GetStats() {
			if p, ok := st.(webrtc.ICECandidatePairStats); ok && p.Nominated && p.CurrentRoundTripTime > 0 {
				rtt = time.Duration(p.CurrentRoundTripTime * float64(time.Second))
			}
		}
		return feedback{loss: loss, rtt: rtt, now: now}
	}
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		switch st {
		case webrtc.PeerConnectionStateConnected:
			if !s.choose("webrtc", sink, fb) {
				_ = pc.Close()
			}
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			switch s.chosen() {
			case "webrtc":
				go s.close()
			case "":
				// No UDP path (Funnel, a blocked network): keep the session for the WebSocket.
				go pc.Close()
			}
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
	return pc.LocalDescription().SDP, nil
}

// forward writes access units to the connected transport until the desktop stream ends; units
// before a transport connects are dropped (connecting asks for a keyframe).
func (s *session) forward() {
	for u := range s.src.Units() {
		s.mu.Lock()
		sink := s.sink
		s.mu.Unlock()
		if sink == nil {
			continue
		}
		if err := sink(u); err != nil {
			break
		}
	}
	if err := s.src.Err(); err != nil {
		log.Printf("stream %s: %v", s.id, err)
		// Tell the viewer why (for example a locked screen) before the connection closes.
		if s.ctl != nil {
			s.ctl.send(map[string]string{"t": "ended", "message": err.Error()})
			time.Sleep(200 * time.Millisecond)
		}
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
			switch p := p.(type) {
			case *rtcp.ReceiverReport:
				s.lossMu.Lock()
				for _, r := range p.Reports {
					s.lossSum += float64(r.FractionLost) / 256
					s.lossReports++
				}
				s.lossMu.Unlock()
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

// adapt runs rate control once a second and applies changes to the running encoder.
func (s *session) adapt() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-tick.C:
			s.mu.Lock()
			fb := s.feedback
			s.mu.Unlock()
			if fb == nil {
				continue // not connected yet
			}
			f := fb(now)
			s.rateMu.Lock()
			t := s.rate.update(f)
			if rateTrace {
				log.Printf("stream %s rate: loss %.3f rtt %s base %s -> %d kbps %dx%d %d fps", s.id, f.loss, f.rtt.Round(time.Millisecond),
					s.rate.baseRTT.Round(time.Millisecond), t.kbps, t.width, t.height, t.fps)
			}
			t = s.apply(t, fmt.Sprintf("loss %.1f%%, rtt %s", f.loss*100, f.rtt.Round(time.Millisecond)))
			s.rateMu.Unlock()
			if s.ctl != nil {
				s.ctl.send(map[string]any{"t": "stats", "kbps": t.kbps, "width": t.width, "height": t.height, "fps": t.fps,
					"loss": math.Round(f.loss*1000) / 1000, "rttMs": f.rtt.Milliseconds()})
			}
		}
	}
}

// apply hands a new target to the running encoder; small bitrate changes are skipped. The caller
// holds rateMu.
func (s *session) apply(t target, why string) target {
	old := s.applied
	if t.width != old.width || t.height != old.height {
		s.src.SetSize(t.width, t.height)
		log.Printf("stream %s: %dx%d at %d kbps (%s)", s.id, t.width, t.height, t.kbps, why)
	}
	if t.fps != old.fps {
		s.src.SetFrameRate(t.fps)
	}
	if math.Abs(float64(t.kbps-old.kbps)) >= float64(old.kbps)/20 {
		s.src.SetBitrate(t.kbps)
	} else {
		t.kbps = old.kbps
	}
	s.applied = t
	s.targetKbps.Store(int32(t.kbps))
	return t
}

// resize refits the running session to the client's new view, in pixels like Request.MaxWidth.
func (s *session) resize(maxW, maxH int) error {
	if maxW < 0 || maxH < 0 || maxW > 8192 || maxH > 8192 {
		return errors.New("resize needs a view of at most 8192 pixels")
	}
	w, h := fit(s.display[0], s.display[1], maxW, maxH, s.profile)
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	s.apply(s.rate.resize(w, h), "view resized")
	return nil
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
		s.mu.Lock()
		pc := s.pc
		s.mu.Unlock()
		if pc != nil {
			_ = pc.Close()
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
