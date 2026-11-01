package stream

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/xrdavies/mobd/internal/desktop"
)

// opus is the audio codec of every session: stereo, as desktop sound is (docs/desktop-audio.md).
var opus = webrtc.RTPCodecCapability{
	MimeType: webrtc.MimeTypeOpus, ClockRate: desktop.AudioSampleRate, Channels: desktop.AudioChannels,
	SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1",
}

// stereoOpus adds stereo to the answer's Opus parameters. Pion echoes the offer's (browsers
// offer mono), and receivers decode to mono unless the answer says the sender is stereo.
func stereoOpus(sdp string) string {
	lines := strings.Split(sdp, "\r\n")
	opusPT := map[string]bool{}
	for _, l := range lines {
		if rest, ok := strings.CutPrefix(l, "a=rtpmap:"); ok {
			if pt, codec, ok := strings.Cut(rest, " "); ok && strings.EqualFold(codec, "opus/48000/2") {
				opusPT[pt] = true
			}
		}
	}
	for i, l := range lines {
		rest, ok := strings.CutPrefix(l, "a=fmtp:")
		if !ok {
			continue
		}
		pt, params, _ := strings.Cut(rest, " ")
		if !opusPT[pt] || strings.Contains(params, "stereo=") {
			continue
		}
		lines[i] = l + ";stereo=1;sprop-stereo=1"
	}
	return strings.Join(lines, "\r\n")
}

// AudioResult answers Request.Audio. "ready" means the Opus track is negotiated; sound starts
// with the "audio" control message. "unavailable" carries the reason for the user.
type AudioResult struct {
	Status          string `json:"status"`
	Reason          string `json:"reason,omitempty"`
	Codec           string `json:"codec,omitempty"`
	SampleRate      int    `json:"sampleRate,omitempty"`
	Channels        int    `json:"channels,omitempty"`
	FrameDurationMs int    `json:"frameDurationMs,omitempty"`
}

// audioStarter is a Source that can capture the desktop's sound.
type audioStarter interface {
	StartAudio(ctx context.Context) (desktop.AudioStream, error)
}

// audioResult decides whether the session offers sound; nil when the client did not ask.
func (s *session) audioResult(req Request) *AudioResult {
	if !req.Audio {
		return nil
	}
	unavailable := func(reason string) *AudioResult { return &AudioResult{Status: "unavailable", Reason: reason} }
	switch {
	case !hasAudioSource(s.source):
		return unavailable("This office computer cannot share its sound yet.")
	case req.Offer == "":
		return unavailable("Sound plays only over a direct (WebRTC) connection; this connection carries the picture only.")
	case !strings.Contains(req.Offer, "m=audio"):
		return unavailable("The app did not ask for an audio track.")
	}
	s.audioWanted = true
	return &AudioResult{Status: "ready", Codec: "opus", SampleRate: desktop.AudioSampleRate, Channels: desktop.AudioChannels,
		FrameDurationMs: int(desktop.AudioFrame / time.Millisecond)}
}

func hasAudioSource(src Source) bool {
	// legacySource always has the method; it can capture only when configured.
	if l, ok := src.(legacySource); ok {
		return l.cfg.Audio != nil
	}
	if d, ok := src.(desktopSource); ok {
		// A conversation's desktop on Linux: only when it runs a sound server now.
		if s, ok := d.Source.(interface{ HasSound() bool }); ok {
			return s.HasSound()
		}
		_, ok := d.Source.(desktop.AudioSource)
		return ok
	}
	_, ok := src.(audioStarter)
	return ok
}

// addAudio adds the Opus track; it shares the video's stream ID, so receivers keep them in sync.
func (s *session) addAudio(pc *webrtc.PeerConnection) error {
	track, err := webrtc.NewTrackLocalStaticSample(opus, "sound", "mobd")
	if err != nil {
		return err
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		return err
	}
	go func() { // RTCP must be read for the interceptors; audio needs nothing from it
		for {
			if _, _, err := sender.ReadRTCP(); err != nil {
				return
			}
		}
	}()
	s.audioTrack = track
	return nil
}

// setAudio turns sound on or off for the client and tells it the outcome on the control channel.
func (s *session) setAudio(on bool) {
	reply := func(state, message string) {
		if s.ctl == nil {
			return
		}
		m := map[string]string{"t": "audio", "state": state}
		if message != "" {
			m["message"] = message
		}
		s.ctl.send(m)
	}
	if !on {
		s.stopAudio()
		reply("off", "")
		return
	}
	if s.audioTrack == nil {
		reply("unavailable", "This live view was opened without sound.")
		return
	}
	if s.chosen() != "webrtc" {
		reply("unavailable", "Sound plays only over a direct (WebRTC) connection; this connection carries the picture only.")
		return
	}
	s.audioMu.Lock()
	if s.audio != nil {
		s.audioMu.Unlock()
		reply("on", "")
		return
	}
	st, err := s.source.(audioStarter).StartAudio(s.ctx)
	if err != nil {
		s.audioMu.Unlock()
		if errors.Is(err, desktop.ErrUnsupported) {
			err = errors.New("This office computer cannot share its sound yet.")
		}
		log.Printf("stream %s: sound: %v", s.id, err)
		reply("unavailable", err.Error())
		return
	}
	s.audio = st
	s.audioMu.Unlock()
	log.Printf("stream %s: sound on", s.id)
	reply("on", "")
	go s.forwardAudio(st, reply)
}

// forwardAudio writes Opus packets to the track. Each packet's duration is the gap since the
// previous one, so packets dropped under load leave a gap rather than shifting the timeline.
func (s *session) forwardAudio(st desktop.AudioStream, reply func(state, message string)) {
	var last time.Duration
	for p := range st.Packets() {
		d := desktop.AudioFrame
		if last != 0 && p.PTS > last && p.PTS-last < time.Second {
			d = p.PTS - last
		}
		last = p.PTS
		if err := s.audioTrack.WriteSample(media.Sample{Data: p.Data, Duration: d}); err != nil {
			break
		}
	}
	s.audioMu.Lock()
	mine := s.audio == st
	if mine {
		s.audio = nil
	}
	s.audioMu.Unlock()
	// Turned off, or the session closed (its context ends the capture before close stops it).
	if !mine || s.ctx.Err() != nil {
		st.Close()
		return
	}
	st.Close()
	message := "The sound stopped."
	if err := st.Err(); err != nil {
		message = err.Error()
	}
	log.Printf("stream %s: sound ended: %s", s.id, message)
	reply("error", message)
}

func (s *session) stopAudio() {
	s.audioMu.Lock()
	st := s.audio
	s.audio = nil
	s.audioMu.Unlock()
	if st != nil {
		st.Close()
		log.Printf("stream %s: sound off", s.id)
	}
}
