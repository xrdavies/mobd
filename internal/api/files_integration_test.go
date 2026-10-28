package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// Opt-in fixture for the real client SDK tests; never uses the operator's data or credentials.
func TestFileTransferIntegrationServer(t *testing.T) {
	path := os.Getenv("MOB_FILE_TEST_FIXTURE")
	if path == "" {
		t.Skip("set MOB_FILE_TEST_FIXTURE to a temporary JSON path")
	}
	s := testServer(t, "")
	credential := pairedClient(t, s)
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	data, err := json.Marshal(map[string]any{"url": server.URL, "token": credential.Token, "pid": os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	t.Logf("isolated file fixture listening on %s (pid %d)", server.URL, os.Getpid())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Hour):
	}
}

func TestFileTransferMethodOverrideAndDirectoryBoundary(t *testing.T) {
	s := testServer(t, "")
	h := authenticatedHandler(t, s)
	location := createFileUpload(t, h, "java.bin", []byte("abc"))
	w := fileRequest(t, h, "POST", location, []byte("abc"), map[string]string{
		"X-HTTP-Method-Override": "PATCH", "Upload-Offset": "0", "Content-Type": "application/offset+octet-stream",
	})
	if w.Code != 204 {
		t.Fatalf("Java PATCH fallback: %d %s", w.Code, w.Body.String())
	}
	finishFileUpload(t, h, location)
	if w := fileRequest(t, h, "POST", location, nil, map[string]string{"X-HTTP-Method-Override": "DELETE"}); w.Code != 404 {
		t.Fatal("unadvertised method override allowed")
	}
	if err := os.Rename(s.cfg.DataDir+"/files", s.cfg.DataDir+"/saved-files"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(s.cfg.DataDir+"/saved-files", s.cfg.DataDir+"/files"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/files", "/files/java.bin"} {
		if w := fileRequest(t, h, "GET", path, nil, nil); w.Code != 500 {
			t.Fatalf("directory link accepted: %s: %d", path, w.Code)
		}
	}
}
