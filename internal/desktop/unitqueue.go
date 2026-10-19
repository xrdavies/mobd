package desktop

import "sync"

// UnitBuffer holds about half a second at 30 fps; a slower consumer loses frames, not latency.
const UnitBuffer = 16

// UnitQueue hands an encoder's access units to the stream consumer. When the consumer falls
// behind, a unit is dropped and every delta after it until the next keyframe (they would decode
// to garbage), and RequestKey is called once so that keyframe comes soon.
type UnitQueue struct {
	RequestKey func() // asks the encoder for a keyframe; must not block

	mu      sync.Mutex
	units   chan AccessUnit
	closed  bool
	needKey bool
}

// NewUnitQueue makes a queue holding UnitBuffer units.
func NewUnitQueue(requestKey func()) *UnitQueue {
	return &UnitQueue{RequestKey: requestKey, units: make(chan AccessUnit, UnitBuffer)}
}

// Units is closed by Close.
func (q *UnitQueue) Units() <-chan AccessUnit { return q.units }

// Deliver queues u without blocking; it reports whether u was queued.
func (q *UnitQueue) Deliver(u AccessUnit) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || (q.needKey && !u.Key) {
		return false
	}
	select {
	case q.units <- u:
		if u.Key {
			q.needKey = false
		}
		return true
	default:
		if !q.needKey {
			q.needKey = true
			if q.RequestKey != nil {
				q.RequestKey()
			}
		}
		return false
	}
}

// Close closes Units; later units are dropped. It is idempotent.
func (q *UnitQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		close(q.units)
	}
}
