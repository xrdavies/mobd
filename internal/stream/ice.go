package stream

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
)

// ICE is the relay's STUN/TURN service (mob-relay docs/media-path.md). mobd holds the entry's
// secret and mints short-lived TURN REST credentials for itself and its paired apps; the relay
// checks them with the same secret.
type ICE struct {
	URLs   []string // stun: and turn: URLs
	Entry  string   // the relay entry ID, the user part of TURN usernames
	Secret string
}

// iceLifetime is how long minted TURN credentials work; a session keeps its allocation past it.
const iceLifetime = 12 * time.Hour

// ICEServer is one entry of GET /stream/ice, as in RTCIceServer.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// Servers lists the STUN servers, and the TURN servers with fresh credentials. Nil without ICE.
func (c *ICE) Servers(now time.Time) []ICEServer {
	if c == nil {
		return nil
	}
	var stun, turn []string
	for _, u := range c.URLs {
		if strings.HasPrefix(u, "turn:") {
			turn = append(turn, u)
		} else {
			stun = append(stun, u)
		}
	}
	var out []ICEServer
	if len(stun) > 0 {
		out = append(out, ICEServer{URLs: stun})
	}
	if len(turn) > 0 && c.Secret != "" {
		user := strconv.FormatInt(now.Add(iceLifetime).Unix(), 10) + ":" + c.Entry
		mac := hmac.New(sha1.New, []byte(c.Secret))
		mac.Write([]byte(user))
		out = append(out, ICEServer{URLs: turn, Username: user, Credential: base64.StdEncoding.EncodeToString(mac.Sum(nil))})
	}
	return out
}

// webrtcServers converts Servers for mobd's own peer connections.
func webrtcServers(servers []ICEServer) []webrtc.ICEServer {
	out := make([]webrtc.ICEServer, 0, len(servers))
	for _, s := range servers {
		w := webrtc.ICEServer{URLs: s.URLs, Username: s.Username}
		if s.Credential != "" {
			w.Credential = s.Credential
		}
		out = append(out, w)
	}
	return out
}

// ICEServers is what GET /stream/ice answers: empty without a relay.
func (m *Manager) ICEServers() []ICEServer {
	servers := m.cfg.ICE.Servers(time.Now())
	if servers == nil {
		return []ICEServer{}
	}
	return servers
}
