package stream

import (
	"testing"
	"time"
)

func run(c *rateController, start time.Time, seconds int, f func(i int) feedback) (target, time.Time) {
	var t target
	now := start
	for i := range seconds {
		now = now.Add(time.Second)
		fb := f(i)
		fb.now = now
		t = c.update(fb)
	}
	return t, now
}

func TestRateRampsOnACleanLinkAndStepsUpTheLadder(t *testing.T) {
	start := time.Now()
	c := newRateController(profiles["auto"], 1920, 1080, start)
	if got := c.current(); got.kbps != 1200 || got.width != 1280 || got.fps != 30 {
		t.Fatalf("start %+v", got)
	}
	got, _ := run(c, start, 20, func(int) feedback { return feedback{rtt: 20 * time.Millisecond} })
	if got.kbps != 6000 || got.width != 1920 || got.height != 1080 {
		t.Fatalf("after 20 clean seconds %+v", got)
	}
}

func TestRateBacksOffOnLossAndQueueing(t *testing.T) {
	start := time.Now()
	c := newRateController(profiles["high"], 1920, 1080, start)
	got, now := run(c, start, 3, func(int) feedback { return feedback{loss: 0.25, rtt: 20 * time.Millisecond} })
	// 25% loss takes 1/8 off each second: 3000 → ~2010, below the 1080p rung.
	if got.kbps < 1950 || got.kbps > 2050 || got.width != 1280 {
		t.Fatalf("heavy loss %+v", got)
	}
	held := got.kbps
	got, now = run(c, now, 3, func(int) feedback { return feedback{loss: 0.05, rtt: 20 * time.Millisecond} })
	if got.kbps != held {
		t.Fatalf("light loss should hold %d, got %+v", held, got)
	}
	got, _ = run(c, now, 1, func(int) feedback { return feedback{rtt: 300 * time.Millisecond} })
	if got.kbps >= held {
		t.Fatalf("queueing delay should back off from %d: %+v", held, got)
	}
}

func TestRateFloorAndLowFrameRate(t *testing.T) {
	start := time.Now()
	c := newRateController(profiles["auto"], 1600, 1000, start)
	got, _ := run(c, start, 30, func(int) feedback { return feedback{loss: 0.5} })
	if got.kbps != minKbps || got.fps != 15 || got.width != 576 || got.height != 360 {
		t.Fatalf("floor %+v", got) // 16:10 view keeps its aspect at the 360p rung
	}
}

func TestRateWaitsBeforeSteppingUp(t *testing.T) {
	start := time.Now()
	c := newRateController(profiles["auto"], 1920, 1080, start)
	c.kbps = 3000 // already above the 1080p rung, but sizes change only after a hold and cooldown
	got := c.update(feedback{now: start.Add(time.Second)})
	if got.width != 1280 {
		t.Fatalf("stepped up immediately: %+v", got)
	}
	got, _ = run(c, start.Add(time.Second), 5, func(int) feedback { return feedback{} })
	if got.width != 1920 {
		t.Fatalf("no step up after hold and cooldown: %+v", got)
	}
}
