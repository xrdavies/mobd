package desktop

import "bytes"

// AnnexB normalizes an encoder's access units to the stream contract (see AccessUnit): an access
// unit delimiter first, SPS and PPS before every IDR, and Key set on IDRs. Encoders that emit
// parameter sets only once (some GStreamer elements) get the cached ones repeated, so a viewer
// can start at any keyframe. VideoToolbox output already meets the contract.
type AnnexB struct {
	sps, pps []byte // last seen, with start codes
}

var (
	startCode = []byte{0, 0, 0, 1}
	aud       = []byte{0, 0, 0, 1, 0x09, 0xf0}
)

// Normalize returns an access unit rewritten as needed and whether it is a keyframe.
func (a *AnnexB) Normalize(data []byte) ([]byte, bool) {
	var hasSPS, hasPPS, idr bool
	var nals [][]byte
	for _, n := range splitNALs(data) {
		if len(n) == 0 {
			continue
		}
		switch n[0] & 0x1f {
		case 9:
			continue // re-added first below
		case 7:
			hasSPS = true
			a.sps = append(append([]byte{}, startCode...), n...)
		case 8:
			hasPPS = true
			a.pps = append(append([]byte{}, startCode...), n...)
		case 5:
			idr = true
		}
		nals = append(nals, n)
	}
	var out bytes.Buffer
	out.Grow(len(data) + len(aud) + len(a.sps) + len(a.pps))
	out.Write(aud)
	if idr && !hasSPS && a.sps != nil {
		out.Write(a.sps)
	}
	if idr && !hasPPS && a.pps != nil {
		out.Write(a.pps)
	}
	for _, n := range nals {
		out.Write(startCode)
		out.Write(n)
	}
	return out.Bytes(), idr
}

// splitNALs returns the NAL units of an Annex B buffer without their start codes.
func splitNALs(data []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+2 < len(data); i++ {
		if data[i] != 0 || data[i+1] != 0 {
			continue
		}
		n := 0
		if data[i+2] == 1 {
			n = 3
		} else if i+3 < len(data) && data[i+2] == 0 && data[i+3] == 1 {
			n = 4
		} else {
			continue
		}
		if start >= 0 {
			out = append(out, trimZeros(data[start:i]))
		}
		start = i + n
		i += n - 1
	}
	if start >= 0 && start <= len(data) {
		out = append(out, data[start:])
	}
	return out
}

// trimZeros drops trailing zero bytes (part of a following 4-byte start code).
func trimZeros(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}
