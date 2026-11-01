package api

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/runner/codex"
	"github.com/xrdavies/mobd/internal/task"
)

func iceAnswer(t *testing.T, s *Server) (servers []map[string]any) {
	t.Helper()
	rr := httptest.NewRecorder()
	authenticatedHandler(t, s).ServeHTTP(rr, httptest.NewRequest("GET", "/stream/ice", nil))
	var body struct {
		IceServers []map[string]any `json:"iceServers"`
		TTL        int              `json:"ttlSeconds"`
	}
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &body) != nil || body.TTL != 43200 || body.IceServers == nil {
		t.Fatalf("GET /stream/ice: %d %s", rr.Code, rr.Body)
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("credentials must not be cached")
	}
	return body.IceServers
}

func TestStreamICE(t *testing.T) {
	plain := testServer(t, "")
	if got := iceAnswer(t, plain); len(got) != 0 || slices.Contains(plain.features(), "streamIce") {
		t.Fatalf("without a relay: %v %v", got, plain.features())
	}
	cfg := config.Config{ListenAddr: ":0", CodexBin: "codex-does-not-exist-for-tests", CodexWorkDir: t.TempDir(), DataDir: t.TempDir(),
		Relay: &config.Relay{TURN: &config.RelayTURN{URLs: []string{"stun:relay.example.com:3478", "turn:relay.example.com:3478?transport=udp"},
			Entry: "office", Secret: strings.Repeat("a", 64)}}}
	s := newTestServer(t, cfg, task.NewStore(), codex.New(cfg))
	got := iceAnswer(t, s)
	if len(got) != 2 || got[1]["username"] == nil || got[1]["credential"] == nil || got[0]["username"] != nil {
		t.Fatalf("ICE servers %v", got)
	}
	if !strings.HasSuffix(got[1]["username"].(string), ":office") {
		t.Fatalf("username %v", got[1]["username"])
	}
	if !slices.Contains(s.features(), "streamIce") {
		t.Fatal("streamIce not advertised")
	}
}
