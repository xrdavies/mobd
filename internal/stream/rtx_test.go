package stream

import (
	"testing"
	"time"
)

func TestRetransmissionBudget(t *testing.T) {
	w := &rtxWindow{}
	now := time.Now()
	for seq := uint16(65000); seq != 100; seq++ { // across the sequence-number wrap
		if !w.allow(seq, false, 1000, now) {
			t.Fatalf("fresh packet %d dropped", seq)
		}
	}
	// ~636 KB fresh this second: resends may use 30% of it plus the burst.
	allowed := 0
	for range 1000 {
		if w.allow(50, false, 1000, now) {
			allowed++
		}
	}
	if fresh := 636; allowed < fresh*30/100 || allowed > fresh*30/100+5 {
		t.Fatalf("%d resends allowed", allowed)
	}
	// Next second: budget is based on the previous second's fresh bytes.
	if !w.allow(60, false, 1000, now.Add(time.Second)) {
		t.Fatal("resend budget did not refill")
	}
}

// RTX packets have their own sequence numbers; a high RTX number must not make fresh video look old.
func TestRetransmissionBudgetKeepsRTXApart(t *testing.T) {
	w := &rtxWindow{}
	now := time.Now()
	for seq := uint16(100); seq < 200; seq++ {
		w.allow(seq, false, 1000, now)
	}
	if !w.allow(40000, true, 1000, now) { // an RTX resend, numbered far above the video
		t.Fatal("RTX resend within budget dropped")
	}
	for seq := uint16(200); seq < 400; seq++ {
		if !w.allow(seq, false, 1000, now) {
			t.Fatalf("fresh video packet %d taken for a resend after RTX", seq)
		}
	}
	allowed := 0
	for range 200 {
		if w.allow(40001, true, 1000, now) {
			allowed++
		}
	}
	if allowed < 85 || allowed > 95 { // 30% of 300 KB fresh, plus the burst, minus the first resend
		t.Fatalf("%d RTX resends allowed", allowed)
	}
}
