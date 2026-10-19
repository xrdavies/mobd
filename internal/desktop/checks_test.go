package desktop

import "testing"

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 999: "999 B", 812e6: "812 MB", 2.1e9: "2.1 GB", 81e9: "81 GB", 16.7e9: "16.7 GB", 250e9: "250 GB", 3e12: "3 TB"} {
		if got := FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
