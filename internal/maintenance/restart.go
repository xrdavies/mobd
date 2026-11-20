package maintenance

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/xrdavies/mobd/internal/config"
)

// Operation states.
const (
	StateRunning    = "running"
	StateDone       = "done"
	StateFailed     = "failed"
	StateRolledBack = "rolled-back"
)

// Operation is one restart (later: upgrade) requested from an app. It is stored on disk so a
// client can read its result after the restart closed its connection.
type Operation struct {
	ID         string     `json:"id"`
	Action     string     `json:"action"`
	State      string     `json:"state"`
	Step       string     `json:"step,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

// Store keeps operations and the config copies used for fallback, in mobd's config directory.
type Store struct{ Dir string }

func (s Store) opsDir() string        { return filepath.Join(s.Dir, "maintenance") }
func (s Store) LastGood() string      { return filepath.Join(s.Dir, "config.last-good.json") }
func (s Store) Rejected() string      { return filepath.Join(s.Dir, "config.rejected.json") }
func (s Store) Snapshot() string      { return filepath.Join(s.Dir, "service.json") }
func (s Store) snapshotCopy() string  { return filepath.Join(s.Dir, "service.previous.json") }
func (s Store) rejectedInfo() string  { return filepath.Join(s.opsDir(), "last-rejected.json") }
func (s Store) path(id string) string { return filepath.Join(s.opsDir(), id+".json") }

// New records a new running operation.
func (s Store) New(action string) (Operation, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return Operation{}, err
	}
	op := Operation{ID: hex.EncodeToString(b), Action: action, State: StateRunning, Step: "checking", StartedAt: time.Now().UTC()}
	return op, s.Save(op)
}

func (s Store) Save(op Operation) error {
	if err := os.MkdirAll(s.opsDir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(op, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(s.path(op.ID), data)
}

func (s Store) Load(id string) (Operation, error) {
	var op Operation
	if id == "" || filepath.Base(id) != id || id == "last-rejected" {
		return op, os.ErrNotExist
	}
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return op, err
	}
	return op, json.Unmarshal(data, &op)
}

// Running is the operation still in progress, if any. One that has been running for longer than
// any restart takes (the helper died) counts as failed.
func (s Store) Running() *Operation {
	entries, _ := os.ReadDir(s.opsDir())
	var ops []Operation
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) != ".json" || name == "last-rejected.json" {
			continue
		}
		if op, err := s.Load(name[:len(name)-len(".json")]); err == nil && op.State == StateRunning {
			ops = append(ops, op)
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].StartedAt.After(ops[j].StartedAt) })
	for _, op := range ops {
		if time.Since(op.StartedAt) > 10*time.Minute {
			s.finish(&op, StateFailed, "the restart did not report back; check mobd's log")
			continue
		}
		return &op
	}
	return nil
}

func (s Store) finish(op *Operation, state, msg string) {
	now := time.Now().UTC()
	op.State, op.Error, op.FinishedAt, op.Step = state, msg, &now, ""
	_ = s.Save(*op)
}

// Rejected is the config change the last restart refused or rolled back, for the apps to show.
type Rejected struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
	// Kept is where the refused config.json was kept, when it was replaced by the last good one.
	Kept string `json:"kept,omitempty"`
}

func (s Store) LastRejected() *Rejected {
	data, err := os.ReadFile(s.rejectedInfo())
	if err != nil {
		return nil
	}
	var r Rejected
	if json.Unmarshal(data, &r) != nil {
		return nil
	}
	return &r
}

func (s Store) reject(reason, kept string) {
	data, _ := json.Marshal(Rejected{At: time.Now().UTC(), Reason: reason, Kept: kept})
	_ = os.MkdirAll(s.opsDir(), 0o700)
	_ = writeFile(s.rejectedInfo(), data)
}

// SaveLastGood records the config a service just started with as the one to fall back to.
func (s Store) SaveLastGood(configPath string) error {
	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		data = []byte("{}\n")
	} else if err != nil {
		return err
	}
	_ = os.Remove(s.rejectedInfo())
	return writeFile(s.LastGood(), data)
}

// Service is the background service: service.Manager, or a fake in tests.
type Service interface {
	Stop() error
	Start(cfg config.Config, args []string, login bool) error
}

// Restart restarts the service on the config args select. A config that fails Check is not
// applied and the running service is left alone. If the new service does not come up, the last
// config that worked replaces config.json (the refused one is kept as config.rejected.json) and
// the service starts again. step reports progress. It returns the final state and why it is not
// done.
func Restart(st Store, svc Service, args []string, login bool, step func(string)) (string, error) {
	if step == nil {
		step = func(string) {}
	}
	step("checking")
	cfg, err := Check(args)
	if err != nil {
		st.reject(err.Error(), "")
		return StateFailed, fmt.Errorf("the config was not applied: %w", err)
	}
	configPath, err := ConfigPath(args)
	if err != nil {
		return StateFailed, err
	}
	// The running service's snapshot: what to fall back to if there is no last-good copy yet.
	if data, err := os.ReadFile(st.Snapshot()); err == nil {
		_ = writeFile(st.snapshotCopy(), data)
	}
	step("restarting")
	if err := svc.Stop(); err != nil {
		return StateFailed, fmt.Errorf("stop: %w", err)
	}
	startErr := svc.Start(cfg, args, login)
	if startErr == nil {
		if err := st.SaveLastGood(configPath); err != nil {
			return StateDone, fmt.Errorf("started, but the last good config could not be saved: %w", err)
		}
		return StateDone, nil
	}
	step("rolling back")
	back := st.LastGood()
	if _, err := os.Stat(back); err != nil {
		back = st.snapshotCopy()
	}
	previous, err := os.ReadFile(back)
	current, _ := os.ReadFile(configPath)
	if err != nil || bytes.Equal(previous, current) {
		st.reject(startErr.Error(), "")
		return StateFailed, fmt.Errorf("mobd did not start (%v) and there is no other config to go back to; it needs attention on the computer", startErr)
	}
	if current != nil {
		_ = writeFile(st.Rejected(), current)
	}
	if err := writeFile(configPath, previous); err != nil {
		return StateFailed, fmt.Errorf("mobd did not start (%v), and restoring the previous config failed: %w", startErr, err)
	}
	st.reject(startErr.Error(), st.Rejected())
	_ = svc.Stop()
	prev, _, err := config.Load(args)
	if err == nil {
		err = svc.Start(prev, args, login)
	}
	if err != nil {
		return StateFailed, fmt.Errorf("mobd did not start with the new config (%v) nor the previous one (%v); it needs attention on the computer", startErr, err)
	}
	return StateRolledBack, fmt.Errorf("mobd did not start with the new config: %w", startErr)
}

// RunOperation runs a stored restart operation and records its steps and result.
func RunOperation(st Store, id string, svc Service, args []string, login bool) error {
	op, err := st.Load(id)
	if err != nil {
		return err
	}
	if op.State != StateRunning {
		return errors.New("operation " + id + " already finished")
	}
	state, rerr := Restart(st, svc, args, login, func(s string) {
		op.Step = s
		_ = st.Save(op)
	})
	msg := ""
	if rerr != nil {
		msg = rerr.Error()
	}
	st.finish(&op, state, msg)
	return rerr
}

func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
