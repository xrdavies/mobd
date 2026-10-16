package desktop

import "errors"

// nalTypes lists the NAL unit types in an Annex B access unit.
func nalTypes(au []byte) []int {
	var types []int
	for i := 0; i+3 < len(au); i++ {
		if au[i] == 0 && au[i+1] == 0 && au[i+2] == 1 {
			types = append(types, int(au[i+3]&0x1f))
			i += 3
		}
	}
	return types
}

// spsSize returns the picture size from the first SPS in an access unit (baseline profile).
func spsSize(au []byte) (int, int, error) {
	var sps []byte
	for i := 0; i+3 < len(au); i++ {
		if au[i] == 0 && au[i+1] == 0 && au[i+2] == 1 && au[i+3]&0x1f == 7 {
			end := len(au)
			for j := i + 4; j+2 < len(au); j++ {
				if au[j] == 0 && au[j+1] == 0 && au[j+2] == 1 {
					end = j
					break
				}
			}
			sps = au[i+4 : end]
			break
		}
	}
	if sps == nil {
		return 0, 0, errors.New("no SPS")
	}
	var rbsp []byte // remove emulation-prevention bytes
	for i := 0; i < len(sps); i++ {
		if i >= 2 && sps[i] == 3 && sps[i-1] == 0 && sps[i-2] == 0 {
			continue
		}
		rbsp = append(rbsp, sps[i])
	}
	r := bitReader{data: rbsp}
	if profile := r.bits(8); profile != 66 && profile != 77 {
		return 0, 0, errors.New("not a baseline/main SPS")
	}
	r.bits(16) // constraint flags, level
	r.ue()     // seq_parameter_set_id
	r.ue()     // log2_max_frame_num_minus4
	switch r.ue() {
	case 0:
		r.ue()
	case 1:
		return 0, 0, errors.New("pic_order_cnt_type 1 not handled")
	}
	r.ue()    // max_num_ref_frames
	r.bits(1) // gaps_in_frame_num_value_allowed_flag
	w := (r.ue() + 1) * 16
	hUnits := r.ue() + 1
	frameMbsOnly := r.bits(1)
	h := hUnits * 16 * (2 - frameMbsOnly)
	if frameMbsOnly == 0 {
		r.bits(1)
	}
	r.bits(1) // direct_8x8_inference_flag
	if r.bits(1) == 1 {
		left, right, top, bottom := r.ue(), r.ue(), r.ue(), r.ue()
		w -= 2 * (left + right)
		h -= 2 * (2 - frameMbsOnly) * (top + bottom)
	}
	if r.err {
		return 0, 0, errors.New("truncated SPS")
	}
	return w, h, nil
}

type bitReader struct {
	data []byte
	pos  int
	err  bool
}

func (r *bitReader) bits(n int) int {
	v := 0
	for range n {
		if r.pos/8 >= len(r.data) {
			r.err = true
			return 0
		}
		v = v<<1 | int(r.data[r.pos/8]>>(7-r.pos%8)&1)
		r.pos++
	}
	return v
}

func (r *bitReader) ue() int {
	zeros := 0
	for r.bits(1) == 0 && !r.err && zeros < 32 {
		zeros++
	}
	return (1<<zeros - 1) + r.bits(zeros)
}
