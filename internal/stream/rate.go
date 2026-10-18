package stream

import (
	"math"
	"time"
)

// Rate control (docs/desktop-streaming.md, M6). Pion's GCC estimate collapsed to its floor under
// 1-2% loss in the M0 spike and recovered slowly, so mobd steers the encoder from the viewer's
// receiver reports instead: loss fraction and round-trip time, once a second. The bitrate ramps
// up multiplicatively while the link is clean and backs off on loss or queueing delay; size and
// frame rate follow the bitrate so keyframes stay small enough for the link.

// rung is one step of the size ladder: used while the bitrate is at least minKbps.
type rung struct {
	width, height int
	minKbps       int
}

var ladder = []rung{{1920, 1080, 2500}, {1280, 720, 1000}, {960, 540, 450}, {640, 360, 0}}

const (
	minKbps       = 150
	lowFPSKbps    = 400 // below this, halve the frame rate (capped at 15)
	heavyLoss     = 0.10
	lightLoss     = 0.02
	sizeHoldTicks = 3               // consecutive ticks above a rung before stepping up
	sizeCooldown  = 5 * time.Second // between size changes (each costs a keyframe)
)

// feedback is one second of the viewer's reports.
type feedback struct {
	loss float64       // fraction lost since the last report, 0..1
	rtt  time.Duration // 0 when unknown
	now  time.Time
}

// target is what the encoder should do now.
type target struct {
	kbps          int
	width, height int
	fps           int
}

type rateController struct {
	p          profile
	maxW, maxH int // fitted to the view and display

	kbps         float64
	baseRTT      time.Duration
	lastDecrease time.Time
	rung         int
	above        int
	lastResize   time.Time
}

func newRateController(p profile, maxW, maxH int, now time.Time) *rateController {
	c := &rateController{p: p, maxW: maxW, maxH: maxH, kbps: float64(p.startKbps), lastResize: now}
	c.rung = c.rungFor(c.kbps)
	return c
}

// size fits a ladder rung inside the session's size, keeping the session's aspect ratio.
func (c *rateController) size(r int) (int, int) {
	w, h := c.maxW, c.maxH
	if ladder[r].width < w {
		h = int(math.Round(float64(h)*float64(ladder[r].width)/float64(w)/2)) * 2
		w = ladder[r].width
	}
	if ladder[r].height < h {
		w = int(math.Round(float64(w)*float64(ladder[r].height)/float64(h)/2)) * 2
		h = ladder[r].height
	}
	return max(w, 16), max(h, 16)
}

func (c *rateController) rungFor(kbps float64) int {
	for i, r := range ladder {
		if kbps >= float64(r.minKbps) {
			return i
		}
	}
	return len(ladder) - 1
}

func (c *rateController) current() target {
	w, h := c.size(c.rung)
	fps := c.p.fps
	if c.kbps < lowFPSKbps {
		fps = min(fps, 15)
	}
	return target{kbps: int(c.kbps), width: w, height: h, fps: fps}
}

// startAt lowers the bitrate (and the size with it) to at most kbps; used when the WebSocket, a slower
// path than WebRTC, carries the session.
func (c *rateController) startAt(kbps int) target {
	c.kbps = math.Min(c.kbps, math.Max(float64(kbps), minKbps))
	c.rung = c.rungFor(c.kbps)
	return c.current()
}

// resize fits the stream to a new view (in device pixels, already fitted to the display and profile).
func (c *rateController) resize(maxW, maxH int) target {
	c.maxW, c.maxH = maxW, maxH
	return c.current()
}

func (c *rateController) update(f feedback) target {
	if f.rtt > 0 && (c.baseRTT == 0 || f.rtt < c.baseRTT) {
		c.baseRTT = f.rtt
	}
	queueing := f.rtt > 0 && c.baseRTT > 0 && f.rtt > c.baseRTT*3/2+50*time.Millisecond
	switch {
	case f.loss > heavyLoss:
		c.kbps *= 1 - f.loss/2
		c.lastDecrease = f.now
	case queueing && f.now.Sub(c.lastDecrease) >= 2*time.Second:
		// The RTT sample comes from ICE checks every couple of seconds; back off once per sample,
		// not once per tick, or one stale reading cuts the rate several times.
		c.kbps *= 0.85
		c.lastDecrease = f.now
	case queueing:
		// hold until the delay has had time to drain
	case f.loss > lightLoss:
		// hold
	case f.now.Sub(c.lastDecrease) >= 2*time.Second:
		// Faster while far below the ceiling, gentler near it.
		step := 1.08
		switch {
		case c.kbps < float64(c.p.kbps)/4:
			step = 1.25
		case c.kbps < float64(c.p.kbps)/2:
			step = 1.15
		}
		c.kbps *= step
	}
	c.kbps = math.Min(math.Max(c.kbps, minKbps), float64(c.p.kbps))
	// Let the RTT baseline drift up slowly, so a route change does not read as congestion forever.
	if c.baseRTT > 0 && f.rtt > c.baseRTT {
		c.baseRTT += (f.rtt - c.baseRTT) / 50
	}

	want := c.rungFor(c.kbps * 1.15) // only step down once clearly below a rung
	if down := c.rungFor(c.kbps); down > c.rung && c.kbps < float64(ladder[c.rung].minKbps)*0.85 {
		c.rung, c.above, c.lastResize = down, 0, f.now
	} else if want < c.rung {
		c.above++
		if c.above >= sizeHoldTicks && f.now.Sub(c.lastResize) >= sizeCooldown {
			c.rung, c.above, c.lastResize = c.rung-1, 0, f.now
		}
	} else {
		c.above = 0
	}
	return c.current()
}
