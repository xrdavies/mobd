package desktop

import (
	"context"
	"sync"
	"time"
)

// Desktop audio (docs/desktop-audio.md): the office computer's playback, Opus encoded at 48 kHz,
// stereo, 20 ms per packet. One capture serves every viewer of a desktop.
const (
	AudioSampleRate = 48000
	AudioChannels   = 2
	AudioFrame      = 20 * time.Millisecond
)

// AudioPacket is one Opus packet of AudioFrame.
type AudioPacket struct {
	Data []byte
	PTS  time.Duration // capture time of its first sample on the host clock
}

// AudioStream is one viewer's share of a desktop's audio capture.
type AudioStream interface {
	// Packets is closed when the stream ends: Close, or the capture stopped (see Err).
	Packets() <-chan AudioPacket
	Err() error
	Close()
}

// AudioSource captures a desktop's playback; ErrUnsupported where it cannot.
type AudioSource interface {
	StartAudio(ctx context.Context) (AudioStream, error)
}

// nativeAudio is a running capture: packets until it ends, then Err.
type nativeAudio interface {
	Packets() <-chan AudioPacket
	Err() error
	Close()
}

// audioQueue bounds what a slow viewer holds: about 200 ms. Older packets are dropped rather
// than played late (a gap is better than delay that never catches up).
const audioQueue = 10

// AudioHub shares one capture between viewers: it starts with the first and stops with the last.
type AudioHub struct {
	start func() (nativeAudio, error)

	mu     sync.Mutex
	native nativeAudio
	subs   map[*audioSub]struct{}
}

func newAudioHub(start func() (nativeAudio, error)) *AudioHub {
	return &AudioHub{start: start, subs: map[*audioSub]struct{}{}}
}

type audioSub struct {
	hub  *AudioHub
	ch   chan AudioPacket
	once sync.Once
	mu   sync.Mutex
	err  error
}

func (s *audioSub) Packets() <-chan AudioPacket { return s.ch }

func (s *audioSub) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *audioSub) Close() { s.hub.leave(s, nil) }

// StartAudio joins the capture, starting it if this is the first viewer.
func (h *AudioHub) StartAudio(ctx context.Context) (AudioStream, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.native == nil {
		n, err := h.start()
		if err != nil {
			return nil, err
		}
		h.native = n
		go h.fanOut(n)
	}
	s := &audioSub{hub: h, ch: make(chan AudioPacket, audioQueue)}
	h.subs[s] = struct{}{}
	if ctx != nil && ctx.Done() != nil {
		go func() {
			<-ctx.Done()
			s.Close()
		}()
	}
	return s, nil
}

// fanOut copies each packet to every viewer, dropping a full viewer's oldest packet.
func (h *AudioHub) fanOut(n nativeAudio) {
	for p := range n.Packets() {
		h.mu.Lock()
		for s := range h.subs {
			select {
			case s.ch <- p:
			default:
				select {
				case <-s.ch:
				default:
				}
				select {
				case s.ch <- p:
				default:
				}
			}
		}
		h.mu.Unlock()
	}
	// The capture ended on its own (or was closed by the last viewer leaving).
	h.mu.Lock()
	if h.native != n {
		h.mu.Unlock()
		return
	}
	h.native = nil
	subs := h.subs
	h.subs = map[*audioSub]struct{}{}
	h.mu.Unlock()
	for s := range subs {
		s.end(n.Err())
	}
}

func (h *AudioHub) leave(s *audioSub, err error) {
	h.mu.Lock()
	_, ok := h.subs[s]
	delete(h.subs, s)
	var stop nativeAudio
	if ok && len(h.subs) == 0 && h.native != nil {
		stop = h.native
		h.native = nil
	}
	h.mu.Unlock()
	if stop != nil {
		stop.Close()
	}
	s.end(err)
}

func (s *audioSub) end(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.ch)
	})
}
