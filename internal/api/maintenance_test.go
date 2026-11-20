package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/maintenance"
	"github.com/xrdavies/mobd/internal/runner/codex"
	"github.com/xrdavies/mobd/internal/task"
)

// maintServer is a server whose user config.json enables codex with a fake program; started
// collects the operations whose helper it was asked to start.
func maintServer(t *testing.T, service bool) (*Server, string, *[]string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700)
	work := filepath.Join(dir, "work")
	os.Mkdir(work, 0o700)
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{"runners":["codex"],"codexBin":"`+bin+`","workdir":"`+work+`"}`), 0o600)
	args := []string{"--config", path}
	cfg, _, err := config.Load(args)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = t.TempDir()
	s := newTestServer(t, cfg, task.NewStore(), codex.New(cfg))
	var started []string
	s.SetMaintenance(Maintenance{Store: maintenance.Store{Dir: dir}, Args: args, Service: service,
		StartHelper: func(id string) error { startedMu.Lock(); started = append(started, id); startedMu.Unlock(); return nil }})
	return s, path, &started
}

func TestConfigIsReadAndChangedRemotely(t *testing.T) {
	s, path, _ := maintServer(t, true)
	if !slices.Contains(s.features(), "maintenance") {
		t.Fatal("feature not advertised")
	}
	var v configView
	json.Unmarshal(doJSON(t, s, http.MethodGet, "/config", "").Body.Bytes(), &v)
	if v.Rev == "" || v.PendingRestart || !v.CanRestart || v.Effective["maxRunning"] == nil || !slices.Contains(v.Editable, "runners") {
		t.Fatalf("config %+v", v)
	}
	if rr := doJSON(t, s, http.MethodPut, "/config", `{"rev":"`+v.Rev+`","changes":{"listen":"0.0.0.0:9"}}`); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "not_editable") {
		t.Fatalf("listen: %d %s", rr.Code, rr.Body)
	}
	if rr := doJSON(t, s, http.MethodPut, "/config", `{"rev":"`+v.Rev+`","changes":{"workdir":"/does/not/exist"}}`); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid: %d %s", rr.Code, rr.Body)
	}
	rr := doJSON(t, s, http.MethodPut, "/config", `{"rev":"`+v.Rev+`","changes":{"maxRunning":3}}`)
	var res struct {
		Rev          string
		NeedsRestart bool
		Diff         []maintenance.Diff
	}
	json.Unmarshal(rr.Body.Bytes(), &res)
	if rr.Code != http.StatusOK || !res.NeedsRestart || len(res.Diff) != 1 {
		t.Fatalf("change: %d %s", rr.Code, rr.Body)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `"maxRunning": 3`) {
		t.Fatalf("not written: %s", data)
	}
	if rr := doJSON(t, s, http.MethodPut, "/config", `{"rev":"`+v.Rev+`","changes":{"maxRunning":4}}`); rr.Code != http.StatusConflict {
		t.Fatalf("stale rev: %d", rr.Code)
	}
	json.Unmarshal(doJSON(t, s, http.MethodGet, "/config", "").Body.Bytes(), &v)
	if !v.PendingRestart || v.Rev != res.Rev {
		t.Fatalf("after change %+v", v)
	}
}

func TestRestartOnlyWhenIdleAndHoldsNewTasks(t *testing.T) {
	s, _, started := maintServer(t, true)
	queued, _ := s.store.Create("busy")
	rr := doJSON(t, s, http.MethodPost, "/maintenance", `{"action":"restart"}`)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "tasks_running") || len(*started) != 0 {
		t.Fatalf("busy: %d %s", rr.Code, rr.Body)
	}
	s.store.UpdateStatus(queued.ID, task.StatusSucceeded, "")
	if rr := doJSON(t, s, http.MethodPost, "/maintenance", `{"action":"upgrade"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("upgrade in phase 1: %d", rr.Code)
	}
	rr = doJSON(t, s, http.MethodPost, "/maintenance", `{"action":"restart"}`)
	var res struct{ OperationID string }
	json.Unmarshal(rr.Body.Bytes(), &res)
	if rr.Code != http.StatusAccepted || len(*started) != 1 || (*started)[0] != res.OperationID {
		t.Fatalf("restart: %d %s %v", rr.Code, rr.Body, *started)
	}
	if rr := doJSON(t, s, http.MethodPost, "/tasks", `{"prompt":"hi"}`); rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "maintenance") {
		t.Fatalf("task during restart: %d %s", rr.Code, rr.Body)
	}
	if rr := doJSON(t, s, http.MethodPost, "/maintenance", `{"action":"restart"}`); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "maintenance_running") {
		t.Fatalf("second restart: %d %s", rr.Code, rr.Body)
	}
	var op maintenance.Operation
	json.Unmarshal(doJSON(t, s, http.MethodGet, "/maintenance/"+res.OperationID, "").Body.Bytes(), &op)
	if op.State != maintenance.StateRunning || op.Action != "restart" {
		t.Fatalf("op %+v", op)
	}
	var health struct{ Maintenance *maintenance.Operation }
	json.Unmarshal(doJSON(t, s, http.MethodGet, "/health", "").Body.Bytes(), &health)
	if health.Maintenance == nil || health.Maintenance.ID != res.OperationID {
		t.Fatalf("health maintenance %+v", health.Maintenance)
	}
	if rr := doJSON(t, s, http.MethodGet, "/maintenance/nope", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown op: %d", rr.Code)
	}
}

func TestRestartRefusesInvalidConfigAndTerminalMobd(t *testing.T) {
	s, path, started := maintServer(t, true)
	os.WriteFile(path, []byte(`{"runners":["nobody"]}`), 0o600)
	if rr := doJSON(t, s, http.MethodPost, "/maintenance", `{"action":"restart"}`); rr.Code != http.StatusUnprocessableEntity || len(*started) != 0 {
		t.Fatalf("invalid: %d %s", rr.Code, rr.Body)
	}
	// The refusal releases the hold: tasks are accepted again.
	if rr := doJSON(t, s, http.MethodPost, "/tasks", `{"prompt":"hi"}`); rr.Code == http.StatusServiceUnavailable {
		t.Fatal("tasks still held")
	}
	fg, _, _ := maintServer(t, false)
	if rr := doJSON(t, fg, http.MethodPost, "/maintenance", `{"action":"restart"}`); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "not_a_service") {
		t.Fatalf("terminal mobd: %d %s", rr.Code, rr.Body)
	}
}

// startedMu guards the fake helper's list: upgrades start it from their own goroutine.
var startedMu sync.Mutex

func startedIDs(p *[]string) []string {
	startedMu.Lock()
	defer startedMu.Unlock()
	return append([]string(nil), *p...)
}

// fakeRelease serves mobd 9.9.9 for this platform.
func fakeRelease(t *testing.T) string {
	t.Helper()
	var key, arch string
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		key, arch = "darwin-aarch64", "darwin_arm64"
	case "linux/amd64":
		key, arch = "linux-x86_64", "linux_amd64"
	case "linux/arm64":
		key, arch = "linux-aarch64", "linux_arm64"
	default:
		t.Skip("no releases for this platform")
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"LICENSE": "", "README.md": "", "mobd": "#!/bin/sh\necho 'mobd 9.9.9'\n"} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	sum := sha256.Sum256(buf.Bytes())
	mux := http.NewServeMux()
	mux.HandleFunc("/mobd/latest.json", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"version":"9.9.9"}`)) })
	mux.HandleFunc("/mobd/v9.9.9/release.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":"9.9.9","platforms":{%q:{"sha256":%q}}}`, key, hex.EncodeToString(sum[:]))
	})
	mux.HandleFunc("/mobd/v9.9.9/mobd_9.9.9_"+arch+".tar.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(buf.Bytes()) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func waitOp(t *testing.T, s *Server, id string, done func(maintenance.Operation) bool) maintenance.Operation {
	t.Helper()
	var op maintenance.Operation
	for i := 0; i < 200; i++ {
		json.Unmarshal(doJSON(t, s, http.MethodGet, "/maintenance/"+id, "").Body.Bytes(), &op)
		if done(op) {
			return op
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("operation stuck: %+v", op)
	return op
}

func TestUpgradeDownloadsThenNeedsAnIdleComputer(t *testing.T) {
	s, _, started := maintServer(t, true)
	exe := filepath.Join(t.TempDir(), "mobd")
	os.WriteFile(exe, []byte("old"), 0o755)
	s.maint.Upgrader = &maintenance.Upgrader{Base: fakeRelease(t), Exe: exe, Current: "0.5.2", Cache: t.TempDir(), Verify: func(string) error { return nil }}

	var rel maintenance.Release
	json.Unmarshal(doJSON(t, s, http.MethodGet, "/maintenance/latest", "").Body.Bytes(), &rel)
	if rel.Latest != "9.9.9" || !rel.Newer || !rel.CanUpgrade {
		t.Fatalf("latest %+v", rel)
	}

	busy, _ := s.store.Create("busy")
	var res struct{ OperationID string }
	json.Unmarshal(doJSON(t, s, http.MethodPost, "/maintenance", `{"action":"upgrade"}`).Body.Bytes(), &res)
	op := waitOp(t, s, res.OperationID, func(o maintenance.Operation) bool { return o.State != maintenance.StateRunning })
	if op.Code != "tasks_running" || op.Version != "9.9.9" || len(startedIDs(started)) != 0 {
		t.Fatalf("busy upgrade %+v", op)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatal("binary replaced while busy")
	}

	s.store.UpdateStatus(busy.ID, task.StatusSucceeded, "")
	json.Unmarshal(doJSON(t, s, http.MethodPost, "/maintenance", `{"action":"upgrade"}`).Body.Bytes(), &res)
	waitOp(t, s, res.OperationID, func(maintenance.Operation) bool { return len(startedIDs(started)) == 1 })
	if ids := startedIDs(started); ids[0] != res.OperationID {
		t.Fatalf("helper started for %v", ids)
	}
	if rr := doJSON(t, s, http.MethodPost, "/tasks", `{"prompt":"hi"}`); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("tasks not held: %d", rr.Code)
	}
}
