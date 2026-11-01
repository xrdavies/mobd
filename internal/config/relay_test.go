package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func secretFile(t *testing.T, mode os.FileMode, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "turn-secret")
	if err := os.WriteFile(p, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func loadErr(t *testing.T, data string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err := Load([]string{"--config", path})
	return err
}

func relayJSON(urls, entry, file string) string {
	return `{"relay":{"turn":{"urls":` + urls + `,"entry":"` + entry + `","secretFile":"` + file + `"}}}`
}

func TestRelayTURNReadsTheSecret(t *testing.T) {
	file := secretFile(t, 0600, testSecret+"\n")
	cfg := loadTest(t, relayJSON(`["stun:relay.example.com:3478","turn:relay.example.com:3478?transport=udp"]`, "office", file))
	if cfg.Relay.TURN.Secret != testSecret || cfg.Relay.TURN.Entry != "office" {
		t.Fatalf("relay %+v", cfg.Relay.TURN)
	}
	// STUN only needs no secret.
	if c := loadTest(t, `{"relay":{"turn":{"urls":["stun:relay.example.com:3478"]}}}`); c.Relay.TURN.Secret != "" {
		t.Fatal("STUN-only config read a secret")
	}
}

func TestRelayTURNRejectsBadConfig(t *testing.T) {
	good := secretFile(t, 0600, testSecret)
	turnURL := `["turn:relay.example.com:3478?transport=udp"]`
	for name, data := range map[string]string{
		"no urls":           relayJSON(`[]`, "office", good),
		"TURN over TLS":     relayJSON(`["turns:relay.example.com:5349"]`, "office", good),
		"TURN over TCP":     relayJSON(`["turn:relay.example.com:3478?transport=tcp"]`, "office", good),
		"no port":           relayJSON(`["turn:relay.example.com"]`, "office", good),
		"bad entry":         relayJSON(turnURL, "Office!", good),
		"no secret file":    relayJSON(turnURL, "office", ""),
		"missing file":      relayJSON(turnURL, "office", filepath.Join(t.TempDir(), "none")),
		"readable by group": relayJSON(turnURL, "office", secretFile(t, 0640, testSecret)),
		"not a key":         relayJSON(turnURL, "office", secretFile(t, 0600, "hunter2")),
	} {
		if err := loadErr(t, data); err == nil || !strings.Contains(err.Error(), "relay.turn") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
