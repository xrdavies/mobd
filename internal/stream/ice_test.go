package stream

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/turn/v5"
	"github.com/pion/webrtc/v4"
)

const iceSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestICEMintsTURNRESTCredentials(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := &ICE{URLs: []string{"stun:relay.example.com:3478", "turn:relay.example.com:3478?transport=udp"}, Entry: "office", Secret: iceSecret}
	s := c.Servers(now)
	if len(s) != 2 || s[0].Username != "" || len(s[1].URLs) != 1 || s[1].URLs[0] != "turn:relay.example.com:3478" {
		t.Fatalf("servers %+v", s)
	}
	exp, entry, _ := strings.Cut(s[1].Username, ":")
	if n, _ := strconv.ParseInt(exp, 10, 64); entry != "office" || time.Unix(n, 0) != now.Add(12*time.Hour) {
		t.Fatalf("username %q", s[1].Username)
	}
	mac := hmac.New(sha1.New, []byte(iceSecret))
	mac.Write([]byte(s[1].Username))
	if s[1].Credential != base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
		t.Fatal("credential is not HMAC-SHA1 of the username")
	}
	if (*ICE)(nil).Servers(now) != nil {
		t.Fatal("no relay should mean no servers")
	}
}

// turnServer runs pion's TURN server with the relay's credential scheme (mob-relay internal/turn).
func turnServer(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := turn.NewServer(turn.ServerConfig{
		Realm: "mob-relay",
		AuthHandler: func(ra *turn.RequestAttributes) (string, []byte, bool) {
			exp, entry, ok := strings.Cut(ra.Username, ":")
			n, err := strconv.ParseInt(exp, 10, 64)
			if !ok || err != nil || entry != "office" || time.Unix(n, 0).Before(time.Now()) {
				return "", nil, false
			}
			mac := hmac.New(sha1.New, []byte(iceSecret))
			mac.Write([]byte(ra.Username))
			return entry, turn.GenerateAuthKey(ra.Username, ra.Realm, base64.StdEncoding.EncodeToString(mac.Sum(nil))), true
		},
		PacketConnConfigs: []turn.PacketConnConfig{{
			PacketConn:            pc,
			RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{RelayAddress: net.IPv4(127, 0, 0, 1), Address: "127.0.0.1"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return pc.LocalAddr().String()
}

// TestSessionThroughTURN: a viewer allowed only relayed candidates (as through Mob relay with no
// direct path) receives video with credentials mobd minted.
func TestSessionThroughTURN(t *testing.T) {
	addr := turnServer(t)
	ice := &ICE{URLs: []string{"turn:" + addr + "?transport=udp"}, Entry: "office", Secret: iceSecret}
	h := harnessWith(t, func(c *Config) { c.ICE = ice })
	servers := h.m.ICEServers()
	if len(servers) != 1 || servers[0].Username == "" {
		t.Fatalf("ICE servers %+v", servers)
	}
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	pc, err := viewerAPI(t, se).NewPeerConnection(webrtc.Configuration{ICEServers: webrtcServers(servers), ICETransportPolicy: webrtc.ICETransportPolicyRelay})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	got := make(chan struct{}, 1)
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if _, _, err := track.ReadRTP(); err == nil {
			got <- struct{}{}
		}
	})
	offer, _ := pc.CreateOffer(nil)
	gathered := webrtc.GatheringCompletePromise(pc)
	_ = pc.SetLocalDescription(offer)
	<-gathered
	sdp := pc.LocalDescription().SDP
	if !strings.Contains(sdp, "typ relay") || strings.Contains(sdp, "typ host") {
		t.Fatalf("viewer offer should carry only relayed candidates:\n%s", sdp)
	}
	a, err := h.m.Open("desk", Request{Offer: sdp})
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: a.Answer}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("no video through TURN")
	}
	for _, st := range pc.GetStats() {
		if p, ok := st.(webrtc.ICECandidatePairStats); ok && p.Nominated {
			if local, ok := pc.GetStats()[p.LocalCandidateID].(webrtc.ICECandidateStats); ok && local.CandidateType != webrtc.ICECandidateTypeRelay {
				t.Fatalf("selected local candidate %s, want relay", local.CandidateType)
			}
		}
	}
}
