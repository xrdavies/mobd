package stream

import (
	"testing"
	"time"
)

func TestRetransmissionBudget(t *testing.T) {
	w := &rtxWindow{}
	now := time.Now()
	for seq := uint16(65000); seq != 100; seq++ { // across the sequence-number wrap
		if !w.allow(seq, 1000, now) {
			t.Fatalf("fresh packet %d dropped", seq)
		}
	}
	// ~636 KB fresh this second: resends may use 30% of it plus the burst.
	allowed := 0
	for range 1000 {
		if w.allow(50, 1000, now) {
			allowed++
		}
	}
	if fresh := 636; allowed < fresh*30/100 || allowed > fresh*30/100+5 {
		t.Fatalf("%d resends allowed", allowed)
	}
	// Next second: budget is based on the previous second's fresh bytes.
	if !w.allow(60, 1000, now.Add(time.Second)) {
		t.Fatal("resend budget did not refill")
	}
}
