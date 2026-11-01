// Command stream-probe is a development client for mobd's live desktop: it opens a WebRTC session
// with a paired device token and reports what arrives. It decodes nothing.
//
//	stream-probe -url http://office-mac:8080 -token TOKEN [-seconds 20] [-max-width 1280] [-quality low] [-audio]
//
// With -audio it asks for the desktop's sound, turns it on, and counts Opus packets: per second,
// and how many carry sound rather than near-silence (more than 100 bytes at 96 kbit/s).
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

func main() {
	addr := flag.String("url", "http://127.0.0.1:8080", "mobd address")
	token := flag.String("token", os.Getenv("MOBD_TOKEN"), "device credential (or MOBD_TOKEN)")
	seconds := flag.Int("seconds", 20, "how long to watch")
	maxWidth := flag.Int("max-width", 0, "view width in pixels (0: full)")
	quality := flag.String("quality", "auto", "auto, high or low")
	pliEvery := flag.Duration("pli", 0, "send a picture-loss report this often (0: never)")
	typeText := flag.String("type", "", "type this text once connected (\\n presses Return)")
	click := flag.String("click", "", "left-click at normalized x,y (0..1) once connected, e.g. 0.53,0.38")
	useWS := flag.Bool("ws", false, "use the WebSocket fallback instead of WebRTC")
	session := flag.String("session", "", "conversation ID whose desktop to watch (Linux: each conversation has its own)")
	resolve := flag.String("resolve", "", "connect to this IP instead of resolving the host (e.g. Funnel's public address)")
	audio := flag.Bool("audio", false, "ask for the desktop's sound and turn it on")
	useICE := flag.Bool("ice", false, "use the STUN/TURN servers from GET /stream/ice (mobd with a relay)")
	relayOnly := flag.Bool("relay-only", false, "with -ice: allow only relayed (TURN) candidates")
	flag.Parse()
	if *resolve != "" {
		// Like curl --resolve: keeps the host name for TLS, dials the given address.
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		http.DefaultTransport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, _ := net.SplitHostPort(addr)
			return dialer.DialContext(ctx, network, net.JoinHostPort(*resolve, port))
		}
	}
	if *token == "" {
		log.Fatal("-token or MOBD_TOKEN is required")
	}
	base := strings.TrimRight(*addr, "/")

	if *useWS {
		watchWS(strings.TrimRight(*addr, "/"), *token, *seconds, *maxWidth, *quality, *session)
		return
	}
	rtc := webrtc.Configuration{}
	if *useICE {
		rtc.ICEServers = fetchICE(base, *token)
		if *relayOnly {
			rtc.ICETransportPolicy = webrtc.ICETransportPolicyRelay
		}
		log.Printf("ICE: %d server entries%s", len(rtc.ICEServers), map[bool]string{true: ", relay only"}[*relayOnly])
	}
	pc, err := webrtc.NewPeerConnection(rtc)
	if err != nil {
		log.Fatal(err)
	}
	defer pc.Close()
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		log.Fatal(err)
	}
	if *audio {
		if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			log.Fatal(err)
		}
	}
	// The control channel carries cursor positions and error reports; the probe sends no input.
	control, err := pc.CreateDataChannel("control", nil)
	if err != nil {
		log.Fatal(err)
	}
	input, err := pc.CreateDataChannel("input", nil)
	if err != nil {
		log.Fatal(err)
	}
	var cursors atomic.Int64
	control.OnMessage(func(msg webrtc.DataChannelMessage) {
		if strings.Contains(string(msg.Data), `"t":"cursor"`) {
			cursors.Add(1)
		} else {
			log.Printf("control: %s", msg.Data)
		}
	})
	var packets, bytesIn, frames atomic.Int64
	var audioPackets, audioLoud, audioBytes, soundOn, firstSound atomic.Int64
	var ssrc atomic.Uint32
	start := time.Now()
	var firstFrame atomic.Int64
	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if t.Kind() == webrtc.RTPCodecTypeAudio {
			log.Printf("audio track: %s/%d/%d %s", t.Codec().MimeType, t.Codec().ClockRate, t.Codec().Channels, t.Codec().SDPFmtpLine)
			for {
				p, _, err := t.ReadRTP()
				if err != nil {
					return
				}
				if audioPackets.Add(1) == 1 && soundOn.Load() > 0 {
					firstSound.Store(int64(time.Since(start)) - soundOn.Load())
				}
				audioBytes.Add(int64(len(p.Payload)))
				if len(p.Payload) > 100 {
					audioLoud.Add(1)
				}
			}
		}
		ssrc.Store(uint32(t.SSRC()))
		for {
			p, _, err := t.ReadRTP()
			if err != nil {
				return
			}
			packets.Add(1)
			bytesIn.Add(int64(len(p.Payload)))
			if p.Marker { // last packet of an access unit
				if frames.Add(1) == 1 {
					firstFrame.Store(int64(time.Since(start)))
				}
			}
		}
	})
	connected := make(chan struct{})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("peer %s after %s", s, time.Since(start).Round(time.Millisecond))
		if s == webrtc.PeerConnectionStateConnected {
			close(connected)
		}
	})
	offer, _ := pc.CreateOffer(nil)
	gathered := webrtc.GatheringCompletePromise(pc)
	_ = pc.SetLocalDescription(offer)
	<-gathered

	body, _ := json.Marshal(map[string]any{"offer": pc.LocalDescription().SDP, "maxWidth": *maxWidth, "quality": *quality, "sessionId": *session, "audio": *audio})
	req, _ := http.NewRequest("POST", base+"/stream", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+*token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		log.Fatalf("POST /stream: %s %s", resp.Status, raw)
	}
	var answer struct {
		Session, Answer string
		Width, Height   int
		Audio           json.RawMessage
	}
	_ = json.Unmarshal(raw, &answer)
	log.Printf("session %s, %dx%d, answered in %s", answer.Session, answer.Width, answer.Height, time.Since(start).Round(time.Millisecond))
	if *audio {
		log.Printf("audio: %s", answer.Audio)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.Answer}); err != nil {
		log.Fatal(err)
	}
	select {
	case <-connected:
	case <-time.After(15 * time.Second):
		log.Fatal("not connected within 15 s")
	}

	if *audio {
		for control.ReadyState() != webrtc.DataChannelStateOpen {
			time.Sleep(50 * time.Millisecond)
		}
		soundOn.Store(int64(time.Since(start)))
		if err := control.SendText(`{"t":"audio","on":true}`); err != nil {
			log.Fatalf("sound on: %v", err)
		}
		log.Printf("sound on requested")
	}

	if *click != "" {
		var x, y float64
		if _, err := fmt.Sscanf(*click, "%g,%g", &x, &y); err != nil {
			log.Fatalf("-click: %v", err)
		}
		time.Sleep(time.Second) // let the input channel open
		for _, down := range []bool{true, false} {
			msg, _ := json.Marshal(map[string]any{"t": "button", "button": "left", "down": down, "x": x, "y": y})
			if err := input.SendText(string(msg)); err != nil {
				log.Fatalf("click: %v", err)
			}
		}
		log.Printf("clicked at %.3f,%.3f", x, y)
	}
	if *typeText != "" {
		time.Sleep(time.Second)
		msg, _ := json.Marshal(map[string]any{"t": "text", "text": strings.ReplaceAll(*typeText, `\n`, "\n")})
		if err := input.SendText(string(msg)); err != nil {
			log.Fatalf("type: %v", err)
		}
		log.Printf("typed %d characters", len(*typeText))
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var lastBytes, lastFrames, lastAudio, lastLoud, lastAudioBytes int64
	nextPLI := time.Now().Add(*pliEvery)
	for i := 0; i < *seconds; i++ {
		<-tick.C
		b, f := bytesIn.Load(), frames.Load()
		path := ""
		stats := pc.GetStats()
		for _, s := range stats {
			if p, ok := s.(webrtc.ICECandidatePairStats); ok && p.Nominated {
				path = fmt.Sprintf("rtt %.0fms", p.CurrentRoundTripTime*1000)
				if c, ok := stats[p.LocalCandidateID].(webrtc.ICECandidateStats); ok {
					path += " via " + c.CandidateType.String()
				}
			}
		}
		sound := ""
		if *audio {
			a, l, ab := audioPackets.Load(), audioLoud.Load(), audioBytes.Load()
			sound = fmt.Sprintf("  audio %2d pkt/s %3d kbps, %2d with sound", a-lastAudio, (ab-lastAudioBytes)*8/1000, l-lastLoud)
			lastAudio, lastLoud, lastAudioBytes = a, l, ab
		}
		fmt.Printf("t=%2ds %5d kbps %3d fps  packets %d  cursor updates %d%s  %s\n", i+1, (b-lastBytes)*8/1000, f-lastFrames, packets.Load(), cursors.Load(), sound, path)
		lastBytes, lastFrames = b, f
		if *pliEvery > 0 && time.Now().After(nextPLI) {
			_ = pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc.Load()}})
			nextPLI = time.Now().Add(*pliEvery)
		}
	}
	fmt.Printf("first frame after %s; %d frames, %d KB\n", time.Duration(firstFrame.Load()).Round(time.Millisecond), frames.Load(), bytesIn.Load()/1024)
	if *audio {
		fmt.Printf("audio: %d packets, %d with sound, first %s after sound on\n", audioPackets.Load(), audioLoud.Load(), time.Duration(firstSound.Load()).Round(time.Millisecond))
	}

	del, _ := http.NewRequest("DELETE", base+"/stream/"+answer.Session, nil)
	del.Header.Set("Authorization", "Bearer "+*token)
	if resp, err := http.DefaultClient.Do(del); err == nil {
		resp.Body.Close()
		log.Printf("DELETE /stream: %s", resp.Status)
	}
	// Closing sends TURN its release without waiting for the answer; exiting at once can lose it,
	// leaving the allocation on the relay until it expires.
	_ = pc.Close()
	time.Sleep(500 * time.Millisecond)
}

// watchWS opens a session without an offer and watches it over GET /stream/ws, acknowledging every
// video message as the apps will.
func watchWS(base, token string, seconds, maxWidth int, quality, session string) {
	start := time.Now()
	body, _ := json.Marshal(map[string]any{"maxWidth": maxWidth, "quality": quality, "sessionId": session})
	req, _ := http.NewRequest("POST", base+"/stream", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		log.Fatalf("POST /stream: %s %s", resp.Status, raw)
	}
	var answer struct {
		Session       string
		Width, Height int
	}
	_ = json.Unmarshal(raw, &answer)
	log.Printf("session %s, %dx%d, answered in %s", answer.Session, answer.Width, answer.Height, time.Since(start).Round(time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds+10)*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/stream/ws?session=" + answer.Session
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		log.Fatalf("GET /stream/ws: %v", err)
	}
	defer c.CloseNow()
	c.SetReadLimit(8 << 20)
	log.Printf("websocket open after %s", time.Since(start).Round(time.Millisecond))
	var bytesIn, frames, keys, cursors atomic.Int64
	var firstFrame atomic.Int64
	go func() {
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageText {
				if strings.Contains(string(data), `"t":"cursor"`) {
					cursors.Add(1)
				} else if strings.Contains(string(data), `"t":"stats"`) && os.Getenv("PROBE_STATS") != "" {
					log.Printf("stats: %s", data)
				} else if !strings.Contains(string(data), `"t":"stats"`) {
					log.Printf("control: %s", data)
				}
				continue
			}
			if len(data) < 10 {
				continue
			}
			ack, _ := json.Marshal(map[string]any{"t": "ack", "pts": int64(binary.BigEndian.Uint64(data[1:9]))})
			_ = c.Write(ctx, websocket.MessageText, ack)
			bytesIn.Add(int64(len(data)))
			if data[9]&1 != 0 {
				keys.Add(1)
			}
			if frames.Add(1) == 1 {
				firstFrame.Store(int64(time.Since(start)))
			}
		}
	}()
	var lastBytes, lastFrames int64
	for i := 0; i < seconds; i++ {
		time.Sleep(time.Second)
		b, f := bytesIn.Load(), frames.Load()
		fmt.Printf("t=%2ds %5d kbps %3d fps  keyframes %d  cursor updates %d\n", i+1, (b-lastBytes)*8/1000, f-lastFrames, keys.Load(), cursors.Load())
		lastBytes, lastFrames = b, f
	}
	fmt.Printf("first frame after %s; %d frames, %d KB\n", time.Duration(firstFrame.Load()).Round(time.Millisecond), frames.Load(), bytesIn.Load()/1024)
	c.Close(websocket.StatusNormalClosure, "")
}

// fetchICE reads GET /stream/ice: the relay's STUN servers and TURN with minted credentials.
func fetchICE(base, token string) []webrtc.ICEServer {
	req, _ := http.NewRequest("GET", base+"/stream/ice", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		IceServers []struct {
			URLs       []string `json:"urls"`
			Username   string   `json:"username"`
			Credential string   `json:"credential"`
		} `json:"iceServers"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&body) != nil {
		log.Fatalf("GET /stream/ice: %s", resp.Status)
	}
	var out []webrtc.ICEServer
	for _, s := range body.IceServers {
		w := webrtc.ICEServer{URLs: s.URLs, Username: s.Username}
		if s.Credential != "" {
			w.Credential = s.Credential
		}
		out = append(out, w)
	}
	return out
}
