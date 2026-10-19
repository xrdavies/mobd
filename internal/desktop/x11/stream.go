//go:build linux

package x11

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/desktop/gst"
)

// ErrNoEncoder: the machine has no working H.264 encoder.
var ErrNoEncoder = fmt.Errorf("%w: the live desktop needs an H.264 encoder; see mobd desktops doctor", desktop.ErrUnsupported)

// firstUnitWait bounds an encoder's first access unit; one that gives none failed to start.
var firstUnitWait = 3 * time.Second

// StartStream captures the screen when it changes, at most opts.FPS times a second, and
// encodes it with the best usable encoder; one that fails to start (a hardware session
// limit) is skipped for a while and the next is tried.
func (b *Backend) StartStream(ctx context.Context, opts desktop.StreamOptions) (desktop.Stream, error) {
	if err := b.alive(); err != nil {
		return nil, err
	}
	usable := b.encoders.Usable()
	if len(usable) == 0 {
		return nil, ErrNoEncoder
	}
	if opts.Width <= 0 || opts.Height <= 0 {
		opts.Width, opts.Height = b.width, b.height
	}
	opts.Width, opts.Height = opts.Width&^1, opts.Height&^1
	opts.FPS = min(max(opts.FPS, 1), 60)
	opts.Kbps = max(opts.Kbps, 100)
	var errs []error
	for _, f := range usable {
		s, err := b.startWith(ctx, f, opts)
		if err == nil {
			return s, nil
		}
		b.encoders.Failed(f.Name, err)
		errs = append(errs, fmt.Errorf("%s: %w", f.Name, err))
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("%w: %w", gst.ErrAllFailed, errors.Join(errs...))
}

// startWith starts a stream on one encoder and waits for its first unit.
func (b *Backend) startWith(ctx context.Context, f gst.Factory, opts desktop.StreamOptions) (*stream, error) {
	enc, err := f.New(gst.EncoderOptions{Width: opts.Width, Height: opts.Height, FPS: opts.FPS, Kbps: opts.Kbps})
	if err != nil {
		return nil, err
	}
	s := &stream{x: b.X11, enc: enc, start: time.Now(), done: make(chan struct{}), fps: opts.FPS,
		width: opts.Width, height: opts.Height, keyNow: make(chan struct{}, 1), first: make(chan struct{}),
		encoderFailed: func(err error) { b.encoders.Failed(f.Name, err) }}
	s.queue = desktop.NewUnitQueue(s.Keyframe)
	b.addStream(s)
	s.wg.Add(2)
	go s.capture(ctx)
	go s.forward()
	select {
	case <-s.first:
		return s, nil
	case <-s.done:
		err = s.Err()
	case <-time.After(firstUnitWait):
		err = errors.New("no output")
	case <-ctx.Done():
		err = ctx.Err()
	}
	s.Close()
	if err == nil {
		err = errors.New("the encoder stopped")
	}
	return nil, err
}

// addStream makes damage acknowledgement follow the fastest running stream.
func (x *X11) addStream(s *stream) {
	x.streamMu.Lock()
	defer x.streamMu.Unlock()
	if x.streams == nil {
		x.streams = map[*stream]bool{}
	}
	x.streams[s] = true
	x.paceLocked()
}

func (x *X11) dropStream(s *stream) {
	x.streamMu.Lock()
	defer x.streamMu.Unlock()
	delete(x.streams, s)
	x.paceLocked()
}

func (x *X11) paceLocked() {
	var every time.Duration
	for s := range x.streams {
		if d := s.interval(); every == 0 || d < every {
			every = d
		}
	}
	x.fastAck.Store(int64(every))
}

// stream is one viewer's live picture: a capture loop feeding an encoder, the encoder's units
// normalized and queued.
type stream struct {
	x      *X11
	enc    gst.Encoder
	queue  *desktop.UnitQueue
	annexb desktop.AnnexB
	start  time.Time
	keyNow chan struct{} // re-push the last frame: a keyframe was asked on a still screen
	first  chan struct{} // closed with the first unit
	done   chan struct{}
	// encoderFailed records a failure of the stream's encoder, so the next stream uses another.
	encoderFailed func(error)
	once          sync.Once
	wg            sync.WaitGroup

	mu            sync.Mutex
	fps           int
	width, height int
	err           error
}

func (s *stream) interval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Second / time.Duration(s.fps)
}

// capture pushes a frame whenever the screen changed since the last one (and the first frame
// at once), paced to the frame rate; a still screen costs nothing.
func (s *stream) capture(ctx context.Context) {
	defer s.wg.Done()
	var last []byte
	var stride int
	seen := s.x.changes.Load()
	push := func(pix []byte) {
		s.enc.Push(gst.Frame{Pix: pix, Stride: stride, Width: s.x.width, Height: s.x.height, PTS: time.Since(s.start)})
	}
	grab := func() bool {
		pix, st, err := s.x.captureRaw(ctx)
		if err != nil {
			s.fail(fmt.Errorf("capture: %w", err))
			return false
		}
		last, stride = pix, st
		push(pix)
		return true
	}
	if !grab() {
		return
	}
	for {
		select {
		case <-s.done:
			return
		case <-ctx.Done():
			s.fail(ctx.Err())
			return
		case <-s.keyNow:
			// A keyframe of a still screen: encode the last frame again.
			if last != nil {
				push(last)
			}
			continue
		case <-time.After(s.interval()):
		}
		if n := s.x.changes.Load(); n != seen {
			seen = n
			if !grab() {
				return
			}
		}
	}
}

// forward normalizes the encoder's units and hands them to the viewer.
func (s *stream) forward() {
	defer s.wg.Done()
	defer s.queue.Close()
	firstUnit := true
	for u := range s.enc.Units() {
		if firstUnit {
			firstUnit = false
			close(s.first)
		}
		data, key := s.annexb.Normalize(u.Data)
		s.mu.Lock()
		w, h := s.width, s.height
		s.mu.Unlock()
		s.queue.Deliver(desktop.AccessUnit{Data: data, Key: key, PTS: time.Since(s.start), Width: w, Height: h})
	}
	if err := s.enc.Err(); err != nil {
		if s.encoderFailed != nil {
			s.encoderFailed(err)
		}
		s.fail(err)
	}
}

func (s *stream) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
	go s.Close()
}

func (s *stream) Units() <-chan desktop.AccessUnit { return s.queue.Units() }
func (s *stream) SetBitrate(kbps int)              { s.enc.SetBitrate(max(kbps, 100)) }

func (s *stream) SetSize(width, height int) {
	width, height = width&^1, height&^1
	if width <= 0 || height <= 0 {
		return
	}
	s.mu.Lock()
	s.width, s.height = width, height
	s.mu.Unlock()
	s.enc.SetSize(width, height)
	s.wakeCapture()
}

func (s *stream) SetFrameRate(fps int) {
	s.mu.Lock()
	s.fps = min(max(fps, 1), 60)
	s.mu.Unlock()
	s.x.streamMu.Lock()
	s.x.paceLocked()
	s.x.streamMu.Unlock()
}

// Keyframe asks for a keyframe now; on a still screen the last frame is encoded again.
func (s *stream) Keyframe() {
	s.enc.ForceKeyframe()
	s.wakeCapture()
}

func (s *stream) wakeCapture() {
	select {
	case s.keyNow <- struct{}{}:
	default:
	}
}

func (s *stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *stream) Close() {
	s.once.Do(func() {
		close(s.done)
		s.x.dropStream(s)
		s.enc.Close() // ends Units, so forward returns
		s.wg.Wait()
	})
}
