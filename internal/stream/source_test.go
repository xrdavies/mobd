package stream

import (
	"errors"
	"testing"
)

type closingSource struct {
	Source
	done chan struct{}
}

func (c closingSource) Done() <-chan struct{} { return c.done }
func (c closingSource) Err() error            { return errors.New("the desktop crashed") }

// A session streams its conversation's desktop and ends when that desktop closes.
func TestSessionFollowsItsDesktopAndEndsWhenItCloses(t *testing.T) {
	var asked string
	src := closingSource{done: make(chan struct{})}
	h := harnessWith(t, func(c *Config) {
		src.Source = legacySource{c}
		c.Source = func(sessionID string) (Source, error) {
			asked = sessionID
			return src, nil
		}
	})
	v, _ := connect(t, h.m, "desk", Request{SessionID: "conv-1", Quality: "auto"})
	waitFor(t, "RTP packets", func() bool { return v.packets.Load() > 5 })
	if asked != "conv-1" {
		t.Fatalf("source asked for %q", asked)
	}
	close(src.done)
	waitFor(t, "session closed with the desktop", func() bool { return h.m.Count() == 0 })
}
