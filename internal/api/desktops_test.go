package api

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/pairing"
	"github.com/xrdavies/mobd/internal/runner/codex"
	"github.com/xrdavies/mobd/internal/task"
)

// perSession is a provider with one asleep desktop, for conversation "asleep".
type perSession struct{ desktop.Provider }

func (perSession) PerSession() bool { return true }
func (perSession) Get(id string) (desktop.Desktop, error) {
	if id != "asleep" {
		return nil, desktop.ErrNoDesktop
	}
	return asleepDesktop{}, nil
}

type asleepDesktop struct{ desktop.Desktop }

func (asleepDesktop) Snapshot(context.Context, desktop.SnapshotOptions) (desktop.Image, desktop.DesktopState, error) {
	return desktop.Image{Data: []byte("last frame"), MIME: "image/jpeg"}, desktop.StateAsleep, nil
}

func TestScreenUsesTheConversationDesktop(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	auth, err := pairing.Open(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, task.NewStore(), auth, perSession{desktop.Shared(desktop.Current())}, codex.New(cfg))
	t.Cleanup(func() { s.Close(); auth.Close() })
	if rr := doJSON(t, s, http.MethodGet, "/screen?sessionId=none", ""); rr.Code != http.StatusConflict {
		t.Fatalf("no desktop: %d %s", rr.Code, rr.Body)
	}
	rr := doJSON(t, s, http.MethodGet, "/screen?sessionId=asleep", "")
	if rr.Code != http.StatusOK || rr.Header().Get("X-Mob-Desktop-State") != "asleep" || rr.Body.String() != "last frame" {
		t.Fatalf("asleep: %d %v %q", rr.Code, rr.Header(), rr.Body)
	}
	if rr := doJSON(t, s, http.MethodPost, "/stream", `{"sessionId":"none","offer":""}`); rr.Code != http.StatusConflict {
		t.Fatalf("stream without desktop: %d %s", rr.Code, rr.Body)
	}
}
