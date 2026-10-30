package stream

import (
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// shapedConn impairs outgoing UDP like a slow link: a token bucket with a drop-tail queue,
// random loss and fixed delay (dummynet does not shape traffic on macOS 26).
type shapedConn struct {
	net.PacketConn
	mu      sync.Mutex
	kbps    int
	loss    float64
	delay   time.Duration
	queued  int
	queue   chan shapedPacket
	offered atomic.Int64
}

type shapedPacket struct {
	data []byte
	addr net.Addr
	at   time.Time
}

const shapeQueueBytes = 64 << 10

func newShapedConn(c net.PacketConn) *shapedConn {
	s := &shapedConn{PacketConn: c, queue: make(chan shapedPacket, 8192)}
	go s.drain()
	return s
}

func (s *shapedConn) set(kbps int, loss float64, delay time.Duration) {
	s.mu.Lock()
	s.kbps, s.loss, s.delay = kbps, loss, delay
	s.mu.Unlock()
}

func (s *shapedConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	s.offered.Add(int64(len(b)))
	s.mu.Lock()
	kbps, loss, delay := s.kbps, s.loss, s.delay
	if loss > 0 && rand.Float64() < loss || kbps > 0 && s.queued+len(b) > shapeQueueBytes {
		s.mu.Unlock()
		return len(b), nil // dropped
	}
	if kbps == 0 && delay == 0 {
		s.mu.Unlock()
		return s.PacketConn.WriteTo(b, addr)
	}
	s.queued += len(b)
	s.mu.Unlock()
	s.queue <- shapedPacket{append([]byte(nil), b...), addr, time.Now().Add(delay)}
	return len(b), nil
}

func (s *shapedConn) drain() {
	next := time.Now()
	for p := range s.queue {
		time.Sleep(time.Until(p.at))
		s.mu.Lock()
		kbps := s.kbps
		s.mu.Unlock()
		if kbps > 0 {
			// Schedule from the previous slot, not from after the sleep: sleep overshoot must not
			// lower the link rate.
			if now := time.Now(); next.Before(now) {
				next = now
			}
			time.Sleep(time.Until(next))
			next = next.Add(time.Duration(len(p.data)*8) * time.Second / time.Duration(kbps*1000))
		}
		_, _ = s.PacketConn.WriteTo(p.data, p.addr)
		s.mu.Lock()
		s.queued -= len(p.data)
		s.mu.Unlock()
	}
}

// The M0 spike froze for most of this run with Pion's GCC; rate control must keep the picture
// moving on a 2 Mbps / 1% loss link and recover when it clears.
func TestRateControlOnAnImpairedLink(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 35 s")
	}
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	shaped := newShapedConn(udp)
	h := harnessWith(t, func(c *Config) { c.UDP = shaped })
	v, _ := connect(t, h.m, "desk", Request{Quality: "auto"})
	// Received kbps and the encoder target, once a second.
	perSecond := func(n int) (out, targets []int64) {
		var offered []int64
		last, lastOff := v.bytes.Load(), shaped.offered.Load()
		for range n {
			time.Sleep(time.Second)
			b, o := v.bytes.Load(), shaped.offered.Load()
			out = append(out, (b-last)*8/1000)
			offered = append(offered, (o-lastOff)*8/1000)
			last, lastOff = b, o
			targets = append(targets, int64(h.last().kbps.Load()))
		}
		t.Logf("offered to the link kbps %v", offered)
		return out, targets
	}
	clean, _ := perSecond(5)
	t.Logf("clean:  received kbps %v, target %d", clean, h.last().kbps.Load())
	shaped.set(2000, 0.01, 20*time.Millisecond)
	impaired, targets := perSecond(15)
	t.Logf("2 Mbps / 1%% / 20 ms: received kbps %v, targets %v", impaired, targets)
	tail := impaired[7:]
	var sum, targetSum int64
	for i, k := range tail {
		if k == 0 {
			t.Fatalf("picture froze in second %d of the impaired tail: %v", i, tail)
		}
		sum += k
		targetSum += targets[7+i]
	}
	// The target probes up and backs off on a saturated link, so a single reading can sit above
	// it (a busy CI runner once read 2499); its mean over the tail must not.
	mean, target := sum/int64(len(tail)), targetSum/int64(len(tail))
	if mean < 1000 || mean > 2100 || target > 2400 {
		t.Fatalf("impaired tail mean %d kbps, mean target %d: should settle under the 2 Mbps link without collapsing", mean, target)
	}
	shaped.set(0, 0, 0)
	recovered, _ := perSecond(12)
	t.Logf("cleared: received kbps %v, target %d", recovered, h.last().kbps.Load())
	if k := int64(h.last().kbps.Load()); k < 2*target || k < 2500 {
		t.Fatalf("did not recover: target %d after clearing (impaired target %d)", k, target)
	}
}
