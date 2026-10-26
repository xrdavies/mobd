package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xrdavies/mobd/internal/desktop"
)

func TestProxyForwardsToolsWithTheTaskToken(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Mob-Task-Token") != "tok" {
			http.Error(w, `{"error":"unknown task token"}`, http.StatusUnauthorized)
			return
		}
		var c desktopCall
		_ = json.NewDecoder(r.Body).Decode(&c)
		got = append(got, r.URL.Path+" "+c.Combo+c.Text)
		switch {
		case strings.HasSuffix(r.URL.Path, "/screenshot"):
			_ = json.NewEncoder(w).Encode(map[string]any{"png": []byte("png"), "meta": desktop.ScreenMeta{ScaleFactor: 2}})
		case strings.HasSuffix(r.URL.Path, "/key"):
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":"unknown key \"hyper\""}`))
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "typed 2 characters"})
		}
	}))
	defer srv.Close()
	t.Setenv("MOB_API", srv.URL+"/")
	t.Setenv("MOB_TASK_ID", "t1")
	// mobd passes the token as a file; the variable is the fallback for older mobd.
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("tok"), 0o600)
	t.Setenv("MOB_TASK_TOKEN_FILE", tokenFile)
	t.Setenv("MOB_TASK_TOKEN", "stale")
	if _, ok := desktopFromEnv().(hostDesktop); !ok {
		t.Fatal("proxy used without MOB_DESKTOP_PROXY")
	}
	t.Setenv("MOB_DESKTOP_PROXY", "1")
	d := desktopFromEnv()
	png, meta, err := d.Screenshot(context.Background())
	if err != nil || string(png) != "png" || meta.ScaleFactor != 2 {
		t.Fatalf("screenshot %q %+v %v", png, meta, err)
	}
	if msg, err := d.TypeText(context.Background(), "hi"); err != nil || msg != "typed 2 characters" {
		t.Fatal(msg, err)
	}
	if err := d.Key("hyper+a"); err == nil || err.Error() != `unknown key "hyper"` {
		t.Fatalf("key error %v", err)
	}
	want := []string{"/internal/tasks/t1/desktop/screenshot ", "/internal/tasks/t1/desktop/type hi", "/internal/tasks/t1/desktop/key hyper+a"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v", got)
	}
}
