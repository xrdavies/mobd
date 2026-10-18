package stream

import (
	"net"
	"regexp"
	"strconv"
	"testing"
)

// mobd binds one UDP socket on all interfaces (streamPort); every session's candidates name that
// port, so a firewall or tailnet rule can allow it.
func TestSessionsShareTheStreamPort(t *testing.T) {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = udp.Close() })
	port := strconv.Itoa(udp.LocalAddr().(*net.UDPAddr).Port)
	h := harnessWith(t, func(c *Config) { c.UDP = udp })
	candidate := regexp.MustCompile(`(?m)^a=candidate:\S+ \d+ udp \d+ \S+ (\d+) typ`)
	for _, client := range []string{"desk", "phone"} {
		v, a := connect(t, h.m, client, Request{Quality: "auto"})
		found := candidate.FindAllStringSubmatch(a.Answer, -1)
		if len(found) == 0 {
			t.Fatalf("%s: no candidates in answer", client)
		}
		for _, m := range found {
			if m[1] != port {
				t.Fatalf("%s: candidate port %s, want %s", client, m[1], port)
			}
		}
		waitFor(t, client+" RTP packets", func() bool { return v.packets.Load() > 10 })
	}
}
