// Command stream-probe is a development client for mobd's live desktop: it opens a WebRTC session
// with a paired device token and reports what arrives. It decodes nothing.
//
//	stream-probe -url http://office-mac:8080 -token TOKEN [-seconds 20] [-max-width 1280] [-quality low]
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

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
	flag.Parse()
	if *token == "" {
		log.Fatal("-token or MOBD_TOKEN is required")
	}
	base := strings.TrimRight(*addr, "/")

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		log.Fatal(err)
	}
	defer pc.Close()
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		log.Fatal(err)
	}
	// The control channel carries cursor positions and error reports; the probe sends no input.
	control, err := pc.CreateDataChannel("control", nil)
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
	var ssrc atomic.Uint32
	start := time.Now()
	var firstFrame atomic.Int64
	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
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

	body, _ := json.Marshal(map[string]any{"offer": pc.LocalDescription().SDP, "maxWidth": *maxWidth, "quality": *quality})
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
	}
	_ = json.Unmarshal(raw, &answer)
	log.Printf("session %s, %dx%d, answered in %s", answer.Session, answer.Width, answer.Height, time.Since(start).Round(time.Millisecond))
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.Answer}); err != nil {
		log.Fatal(err)
	}
	select {
	case <-connected:
	case <-time.After(15 * time.Second):
		log.Fatal("not connected within 15 s")
	}

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var lastBytes, lastFrames int64
	nextPLI := time.Now().Add(*pliEvery)
	for i := 0; i < *seconds; i++ {
		<-tick.C
		b, f := bytesIn.Load(), frames.Load()
		path := ""
		for _, s := range pc.GetStats() {
			if p, ok := s.(webrtc.ICECandidatePairStats); ok && p.Nominated {
				path = fmt.Sprintf("rtt %.0fms", p.CurrentRoundTripTime*1000)
			}
		}
		fmt.Printf("t=%2ds %5d kbps %3d fps  packets %d  cursor updates %d  %s\n", i+1, (b-lastBytes)*8/1000, f-lastFrames, packets.Load(), cursors.Load(), path)
		lastBytes, lastFrames = b, f
		if *pliEvery > 0 && time.Now().After(nextPLI) {
			_ = pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc.Load()}})
			nextPLI = time.Now().Add(*pliEvery)
		}
	}
	fmt.Printf("first frame after %s; %d frames, %d KB\n", time.Duration(firstFrame.Load()).Round(time.Millisecond), frames.Load(), bytesIn.Load()/1024)

	del, _ := http.NewRequest("DELETE", base+"/stream/"+answer.Session, nil)
	del.Header.Set("Authorization", "Bearer "+*token)
	if resp, err := http.DefaultClient.Do(del); err == nil {
		resp.Body.Close()
		log.Printf("DELETE /stream: %s", resp.Status)
	}
}
