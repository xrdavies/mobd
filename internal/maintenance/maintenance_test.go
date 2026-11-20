package maintenance

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/config"
)

// setup writes a config.json enabling codex with a fake program, and returns the args selecting it.
func setup(t *testing.T, extra string) (string, []string, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700)
	work := filepath.Join(dir, "work")
	os.Mkdir(work, 0o700)
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{"listen":"127.0.0.1:18080","runners":["codex"],"codexBin":"`+bin+`","workdir":"`+work+`"`+extra+"}\n"), 0o600)
	return dir, []string{"--config", path}, path
}

func rev(t *testing.T, args []string) string {
	f, err := Read(args)
	if err != nil {
		t.Fatal(err)
	}
	return f.Rev
}

func TestChangeValidatesAndKeepsOtherKeys(t *testing.T) {
	dir, args, path := setup(t, `,"corsOrigins":["tauri://localhost"]`)
	f, _ := Read(args)
	if _, ok := f.Values["listen"]; ok || f.Values["codexBin"] == nil {
		t.Fatalf("read %v", f.Values)
	}
	if _, _, err := Change(args, f.Rev, map[string]json.RawMessage{"listen": json.RawMessage(`"0.0.0.0:1"`)}); !errors.As(err, new(*NotEditableError)) {
		t.Fatalf("listen editable: %v", err)
	}
	if _, _, err := Change(args, "stale", map[string]json.RawMessage{"maxRunning": json.RawMessage(`2`)}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale: %v", err)
	}
	before, _ := os.ReadFile(path)
	missing := filepath.Join(dir, "nope")
	if _, _, err := Change(args, f.Rev, map[string]json.RawMessage{"workdir": json.RawMessage(`"` + missing + `"`)}); !errors.As(err, new(*InvalidError)) || !strings.Contains(err.Error(), "not a folder") {
		t.Fatalf("missing workdir: %v", err)
	}
	if _, _, err := Change(args, f.Rev, map[string]json.RawMessage{"runners": json.RawMessage(`["codex","pi"]`), "piBin": json.RawMessage(`"/no/pi"`)}); err == nil || !strings.Contains(err.Error(), "pi is enabled") {
		t.Fatalf("missing pi: %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("an invalid change was written")
	}
	newRev, diff, err := Change(args, f.Rev, map[string]json.RawMessage{"maxRunning": json.RawMessage(`2`), "codexMode": json.RawMessage(`null`)})
	if err != nil || len(diff) != 1 || diff[0].Key != "maxRunning" || newRev == f.Rev {
		t.Fatalf("change %v %v %q", diff, err, newRev)
	}
	var raw map[string]any
	data, _ := os.ReadFile(path)
	json.Unmarshal(data, &raw)
	if raw["maxRunning"] != float64(2) || raw["listen"] != "127.0.0.1:18080" || raw["corsOrigins"] == nil {
		t.Fatalf("written %s", data)
	}
	if rev(t, args) != newRev {
		t.Fatal("rev does not match the file")
	}
	if _, err := os.Stat(path + ".candidate"); !os.IsNotExist(err) {
		t.Fatal("candidate left behind")
	}
}

func TestOverriddenKeys(t *testing.T) {
	t.Setenv("MOBD_CLAUDE_BIN", "/x/claude")
	got := Overridden([]string{"--config", "c.json", "--workdir=/w"})
	if strings.Join(got, ",") != "claudeBin,workdir" {
		t.Fatalf("overridden %v", got)
	}
}

type fakeService struct {
	calls []string
	fail  func(cfg config.Config) error
}

func (f *fakeService) Stop() error { f.calls = append(f.calls, "stop"); return nil }
func (f *fakeService) Start(cfg config.Config, _ []string, _ bool) error {
	f.calls = append(f.calls, "start")
	if f.fail != nil {
		return f.fail(cfg)
	}
	return nil
}

func TestRestartSavesLastGood(t *testing.T) {
	dir, args, path := setup(t, "")
	st := Store{Dir: dir}
	svc := &fakeService{}
	state, err := Restart(st, svc, args, false, nil)
	if err != nil || state != StateDone || strings.Join(svc.calls, ",") != "stop,start" {
		t.Fatalf("%s %v %v", state, err, svc.calls)
	}
	good, _ := os.ReadFile(st.LastGood())
	cur, _ := os.ReadFile(path)
	if string(good) != string(cur) {
		t.Fatal("last good is not the started config")
	}
}

func TestInvalidConfigNeverStopsTheService(t *testing.T) {
	dir, args, path := setup(t, "")
	os.WriteFile(path, []byte(`{"runners":["nobody"]}`), 0o600)
	st := Store{Dir: dir}
	svc := &fakeService{}
	state, err := Restart(st, svc, args, false, nil)
	if state != StateFailed || err == nil || len(svc.calls) != 0 {
		t.Fatalf("%s %v %v", state, err, svc.calls)
	}
	if r := st.LastRejected(); r == nil || !strings.Contains(r.Reason, "unknown runner") {
		t.Fatalf("rejected %+v", r)
	}
}

func TestFailedStartRollsBackToLastGood(t *testing.T) {
	dir, args, path := setup(t, "")
	st := Store{Dir: dir}
	good, _ := os.ReadFile(path)
	st.SaveLastGood(path)
	// A valid config whose service does not come up (here: maxRunning 3 makes the fake fail).
	f, _ := Read(args)
	if _, _, err := Change(args, f.Rev, map[string]json.RawMessage{"maxRunning": json.RawMessage(`3`)}); err != nil {
		t.Fatal(err)
	}
	svc := &fakeService{fail: func(cfg config.Config) error {
		if cfg.MaxRunning == 3 {
			return errors.New("did not answer")
		}
		return nil
	}}
	op, _ := st.New("restart")
	err := RunOperation(st, op.ID, svc, args, false, "")
	op, _ = st.Load(op.ID)
	if op.State != StateRolledBack || err == nil || !strings.Contains(op.Error, "did not answer") || op.FinishedAt == nil {
		t.Fatalf("op %+v err %v", op, err)
	}
	if cur, _ := os.ReadFile(path); string(cur) != string(good) {
		t.Fatalf("config not restored: %s", cur)
	}
	if kept, _ := os.ReadFile(st.Rejected()); !strings.Contains(string(kept), `"maxRunning": 3`) {
		t.Fatalf("refused config not kept: %s", kept)
	}
	if strings.Join(svc.calls, ",") != "stop,start,stop,start" {
		t.Fatalf("calls %v", svc.calls)
	}
	if st.Running() != nil {
		t.Fatal("finished operation still running")
	}
}

func TestNoFallbackReportsAttentionNeeded(t *testing.T) {
	dir, args, _ := setup(t, "")
	st := Store{Dir: dir}
	svc := &fakeService{fail: func(config.Config) error { return errors.New("port busy") }}
	state, err := Restart(st, svc, args, false, nil)
	if state != StateFailed || err == nil || !strings.Contains(err.Error(), "needs attention") {
		t.Fatalf("%s %v", state, err)
	}
}

func TestStaleRunningOperationCountsAsFailed(t *testing.T) {
	st := Store{Dir: t.TempDir()}
	op, _ := st.New("restart")
	if r := st.Running(); r == nil || r.ID != op.ID {
		t.Fatal("running operation not found")
	}
	op.StartedAt = time.Now().Add(-time.Hour)
	st.Save(op)
	if st.Running() != nil {
		t.Fatal("stale operation still running")
	}
	if op, _ = st.Load(op.ID); op.State != StateFailed {
		t.Fatalf("stale op %+v", op)
	}
	if _, err := st.Load("../x"); err == nil {
		t.Fatal("path outside the store loaded")
	}
}
