package desktop

import (
	"bytes"
	"testing"
)

func TestUnitQueueDropsDeltasUntilTheNextKeyframe(t *testing.T) {
	requests := 0
	q := NewUnitQueue(func() { requests++ })
	if !q.Deliver(AccessUnit{Key: true}) {
		t.Fatal("first keyframe dropped")
	}
	for range UnitBuffer - 1 {
		q.Deliver(AccessUnit{})
	}
	// Full: this delta is dropped and a keyframe requested once; later deltas wait for a key.
	if q.Deliver(AccessUnit{}) || q.Deliver(AccessUnit{}) || requests != 1 {
		t.Fatalf("overflow: requests %d", requests)
	}
	for range UnitBuffer {
		<-q.Units()
	}
	if q.Deliver(AccessUnit{}) {
		t.Fatal("delta accepted before a keyframe")
	}
	if !q.Deliver(AccessUnit{Key: true}) || !q.Deliver(AccessUnit{}) {
		t.Fatal("stream did not resume at the keyframe")
	}
	q.Close()
	q.Close()
	if q.Deliver(AccessUnit{Key: true}) {
		t.Fatal("delivered after close")
	}
}

func TestAnnexBAddsDelimiterAndRepeatsParameterSets(t *testing.T) {
	sps, pps := []byte{0x67, 0x42, 0xc0, 0x1f}, []byte{0x68, 0xce}
	idr, delta := []byte{0x65, 1, 2}, []byte{0x41, 3}
	sc := []byte{0, 0, 0, 1}
	join := func(nals ...[]byte) []byte {
		var b bytes.Buffer
		for _, n := range nals {
			b.Write(sc)
			b.Write(n)
		}
		return b.Bytes()
	}
	var a AnnexB
	// First keyframe carries its parameter sets (3-byte start code on the slice).
	first := append(join(sps, pps), append([]byte{0, 0, 1}, idr...)...)
	out, key := a.Normalize(first)
	if !key || !bytes.Equal(out, join([]byte{0x09, 0xf0}, sps, pps, idr)) {
		t.Fatalf("first keyframe % x", out)
	}
	out, key = a.Normalize(join(delta))
	if key || !bytes.Equal(out, join([]byte{0x09, 0xf0}, delta)) {
		t.Fatalf("delta % x", out)
	}
	// A later IDR without parameter sets gets the cached ones; an existing AUD is not doubled.
	out, key = a.Normalize(join([]byte{0x09, 0xf0}, idr))
	if !key || !bytes.Equal(out, join([]byte{0x09, 0xf0}, sps, pps, idr)) {
		t.Fatalf("bare IDR % x", out)
	}
	if got := nalTypes(out); len(got) != 4 || got[0] != 9 || got[1] != 7 || got[2] != 8 || got[3] != 5 {
		t.Fatalf("nal types %v", got)
	}
}
