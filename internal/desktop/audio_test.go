package desktop

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeNative struct {
	ch   chan AudioPacket
	once sync.Once
	err  error
}

func (f *fakeNative) Packets() <-chan AudioPacket { return f.ch }
func (f *fakeNative) Err() error                  { return f.err }
func (f *fakeNative) Close()                      { f.once.Do(func() { close(f.ch) }) }

func TestAudioHubSharesOneCaptureAndStopsWithTheLastViewer(t *testing.T) {
	var starts atomic.Int32
	var native *fakeNative
	hub := NewAudioHub(func() (AudioStream, error) {
		starts.Add(1)
		native = &fakeNative{ch: make(chan AudioPacket, 64)}
		return native, nil
	})
	a, _ := hub.StartAudio(context.Background())
	b, _ := hub.StartAudio(context.Background())
	if starts.Load() != 1 {
		t.Fatalf("captures started: %d", starts.Load())
	}
	native.ch <- AudioPacket{Data: []byte{1}, PTS: time.Millisecond}
	for _, s := range []AudioStream{a, b} {
		select {
		case p := <-s.Packets():
			if p.Data[0] != 1 {
				t.Fatalf("packet %v", p)
			}
		case <-time.After(time.Second):
			t.Fatal("a viewer got no packet")
		}
	}
	a.Close()
	if _, ok := <-a.Packets(); ok {
		t.Fatal("closed viewer still open")
	}
	b.Close()
	if _, ok := <-native.ch; ok {
		t.Fatal("capture still running after the last viewer left")
	}
	// The next viewer starts a new capture.
	c, _ := hub.StartAudio(context.Background())
	defer c.Close()
	if starts.Load() != 2 {
		t.Fatalf("captures started: %d", starts.Load())
	}
}

func TestAudioHubEndsViewersWithTheCaptureError(t *testing.T) {
	native := &fakeNative{ch: make(chan AudioPacket, 1), err: errors.New("permission revoked")}
	hub := NewAudioHub(func() (AudioStream, error) { return native, nil })
	s, _ := hub.StartAudio(context.Background())
	native.Close()
	if _, ok := <-s.Packets(); ok {
		t.Fatal("viewer not ended")
	}
	if s.Err() == nil || s.Err().Error() != "permission revoked" {
		t.Fatalf("err %v", s.Err())
	}
}

func TestAudioHubDropsOldPacketsForASlowViewer(t *testing.T) {
	native := &fakeNative{ch: make(chan AudioPacket)}
	hub := NewAudioHub(func() (AudioStream, error) { return native, nil })
	s, _ := hub.StartAudio(context.Background())
	defer s.Close()
	for i := range audioQueue + 5 {
		native.ch <- AudioPacket{PTS: time.Duration(i)}
	}
	time.Sleep(50 * time.Millisecond)
	if p := <-s.Packets(); p.PTS != 5 {
		t.Fatalf("oldest kept packet %d, want 5", p.PTS)
	}
}

func TestStartAudioErrorLeavesNoCapture(t *testing.T) {
	hub := NewAudioHub(func() (AudioStream, error) { return nil, ErrUnsupported })
	if _, err := hub.StartAudio(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err %v", err)
	}
}
