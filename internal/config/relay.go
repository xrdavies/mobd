package config

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Relay connects mobd to a self-hosted Mob relay (mob-relay docs/media-path.md).
type Relay struct {
	// TURN lets live views through the relay use WebRTC: STUN for direct connections, TURN to
	// relay media when apps and mobd cannot reach each other.
	TURN *RelayTURN `json:"turn,omitempty"`
}

// RelayTURN names the relay's STUN/TURN service and the entry secret mobd mints credentials with.
type RelayTURN struct {
	// URLs such as stun:relay.example.com:3478 and turn:relay.example.com:3478?transport=udp.
	URLs []string `json:"urls"`
	// Entry is this mobd's entry ID in the relay's server.json.
	Entry string `json:"entry"`
	// SecretFile holds the entry's turnSecretFile contents (64 hex characters, mode 0600 or 0400).
	SecretFile string `json:"secretFile"`
	// Secret is read from SecretFile when the configuration loads; never written out.
	Secret string `json:"-"`
}

var (
	relayEntry = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)
	hexSecret  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// validateRelay checks relay.turn and reads its secret.
func validateRelay(cfg *Config) error {
	if cfg.Relay == nil || cfg.Relay.TURN == nil {
		return nil
	}
	t := cfg.Relay.TURN
	if len(t.URLs) == 0 || len(t.URLs) > 4 {
		return errors.New("relay.turn.urls must list 1–4 stun: or turn: URLs")
	}
	turn := false
	for _, u := range t.URLs {
		scheme, rest, ok := strings.Cut(u, ":")
		host, query, _ := strings.Cut(rest, "?")
		if !ok || (scheme != "stun" && scheme != "turn") || strings.HasPrefix(rest, "//") {
			return errors.New("relay.turn.urls must be stun:HOST:PORT or turn:HOST:PORT?transport=udp")
		}
		if _, port, err := net.SplitHostPort(host); err != nil || port == "" {
			return errors.New("relay.turn.urls need host:port, e.g. turn:relay.example.com:3478?transport=udp")
		}
		if scheme == "turn" {
			if query != "" && query != "transport=udp" {
				return errors.New("relay.turn.urls: only UDP TURN is supported (?transport=udp)")
			}
			turn = true
		}
	}
	if turn && !relayEntry.MatchString(t.Entry) {
		return errors.New("relay.turn.entry must be the relay entry ID (lowercase letters, digits, hyphens)")
	}
	if !turn {
		return nil
	}
	path, err := filepath.Abs(expandHome(t.SecretFile))
	if t.SecretFile == "" || err != nil {
		return errors.New("relay.turn.secretFile is required for turn: URLs")
	}
	t.SecretFile = path
	secret, err := readSecret(path)
	if err != nil {
		return err
	}
	t.Secret = secret
	return nil
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

func readSecret(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("relay.turn.secretFile cannot be opened")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "", errors.New("relay.turn.secretFile must be a regular file")
	}
	if err := checkSecretFile(st); err != nil {
		return "", err
	}
	b, err := io.ReadAll(io.LimitReader(f, 67))
	secret := strings.TrimSuffix(string(b), "\n")
	if err != nil || !hexSecret.MatchString(secret) {
		return "", errors.New("relay.turn.secretFile must contain the 64-character key made by mobr token")
	}
	return secret, nil
}
