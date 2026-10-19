package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/task"
)

func TestDesktopsList(t *testing.T) {
	p := &scripted{}
	s, _ := dispatchServer(t, p, 1, "")
	a, _ := s.store.Create("book flights")
	b, _ := s.store.Create("quarterly report")
	p.list = []desktop.Info{
		{SessionID: a.SessionID, State: desktop.StateAsleep, MemoryBytes: 100},
		{SessionID: b.SessionID, State: desktop.StateRunning, MemoryBytes: 900},
		{SessionID: "gone", State: desktop.StateFailed},
	}
	p.res = desktop.Resources{Memory: desktop.MemoryInfo{Level: desktop.LevelLow, AvailableBytes: 2e9, TotalBytes: 16e9}}
	rr := doJSON(t, s, http.MethodGet, "/desktops", "")
	var got struct {
		Desktops []desktop.Info
		Memory   desktop.MemoryInfo
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Desktops) != 2 || got.Desktops[0].SessionID != b.SessionID || got.Desktops[1].Title == "" ||
		got.Memory.Level != desktop.LevelLow {
		t.Fatalf("GET /desktops %s", rr.Body)
	}
	// One shared desktop: an empty list, no memory or disk.
	shared := testServer(t, "")
	if rr := doJSON(t, shared, http.MethodGet, "/desktops", ""); strings.TrimSpace(rr.Body.String()) != `{"desktops":[]}` {
		t.Errorf("shared: %s", rr.Body)
	}
	if rr := doJSON(t, shared, http.MethodPost, "/desktops/x", ""); rr.Code != http.StatusNotImplemented {
		t.Errorf("shared POST: %d", rr.Code)
	}
}

func TestOpenDesktop(t *testing.T) {
	p := &scripted{}
	s, _ := dispatchServer(t, p, 1, "")
	a, _ := s.store.Create("a")
	if rr := doJSON(t, s, http.MethodPost, "/desktops/nope", ""); rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), `"session_not_found"`) {
		t.Errorf("unknown: %d %s", rr.Code, rr.Body)
	}
	p.refuse = func(string) desktop.QueueReason { return desktop.WaitMemory }
	if rr := doJSON(t, s, http.MethodPost, "/desktops/"+a.SessionID, ""); rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), `"low_memory"`) {
		t.Errorf("low memory: %d %s", rr.Code, rr.Body)
	}
	p.refuse = nil
	rr := doJSON(t, s, http.MethodPost, "/desktops/"+a.SessionID, "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"display":":101"`) || !strings.Contains(rr.Body.String(), `"title":"a"`) {
		t.Errorf("open: %d %s", rr.Code, rr.Body)
	}
	if !slices.Contains(p.Calls(), "ensure "+a.SessionID) || p.released != 1 {
		t.Errorf("calls %q released %d", p.Calls(), p.released)
	}
}

func TestCloseDesktopStopsTheTurn(t *testing.T) {
	p := &scripted{}
	s, _ := dispatchServer(t, p, 1, `while [ ! -f release ]; do sleep 0.01; done`)
	a := postTask(t, s, `{"prompt":"a"}`)
	waitFor(t, "running", func() bool { return status(t, s, a.ID).Status == task.StatusRunning })
	if rr := doJSON(t, s, http.MethodDelete, "/desktops/"+a.SessionID, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("close %d %s", rr.Code, rr.Body)
	}
	if !slices.Contains(p.Calls(), "close "+a.SessionID+" user") {
		t.Errorf("calls %q", p.Calls())
	}
	if got := waitTask(t, s, a.ID); got.Status != task.StatusCancelled {
		t.Errorf("turn %+v", got)
	}
	if rr := doJSON(t, s, http.MethodDelete, "/desktops/nope", ""); rr.Code != http.StatusNotFound {
		t.Errorf("unknown: %d", rr.Code)
	}
}

func TestDesktopEventsReachClients(t *testing.T) {
	p := &scripted{}
	s, _ := dispatchServer(t, p, 1, "")
	a, _ := s.store.Create("titled")
	after := s.hub.cursor()
	p.publish(desktop.Event{Type: "desktop", Desktop: &desktop.Info{SessionID: a.SessionID, State: desktop.StateAsleep}})
	p.publish(desktop.Event{Type: "memory", Memory: &desktop.MemoryInfo{Level: desktop.LevelCritical}})
	p.publish(desktop.Event{Type: "disk", Disk: &desktop.DiskInfo{Level: desktop.LevelLow}})
	events, _, _, _ := s.hub.since(after)
	var got []string
	for _, e := range events {
		if e.Type == "desktop" || e.Type == "memory" || e.Type == "disk" {
			got = append(got, e.Type+" "+string(e.Data))
		}
	}
	if len(got) != 3 || !strings.Contains(got[0], `"state":"asleep"`) || !strings.Contains(got[0], `"title":"titled"`) ||
		!strings.Contains(got[1], `memory {"level":"critical"`) || !strings.Contains(got[2], `disk {"level":"low"`) {
		t.Errorf("events %q", got)
	}
}

func TestTurnWaitsForMemory(t *testing.T) {
	p := &scripted{refuse: func(string) desktop.QueueReason { return desktop.WaitMemory }}
	s, _ := dispatchServer(t, p, 4, "")
	if a := postTask(t, s, `{"prompt":"a"}`); a.Status != task.StatusQueued || a.QueueReason != "memory" {
		t.Fatalf("a: %+v", a)
	}
}

func TestHealthWithSessionDesktops(t *testing.T) {
	p := &scripted{}
	s, _ := dispatchServer(t, p, 4, "")
	s.cfg.MaxDesktops = 6
	p.list = []desktop.Info{
		{SessionID: "a", State: desktop.StateAsleep, MemoryBytes: 1e9, DiskBytes: 2e8},
		{SessionID: "b", State: desktop.StateRunning, MemoryBytes: 2e9, DiskBytes: 1e8},
	}
	p.res = desktop.Resources{
		Memory: desktop.MemoryInfo{Level: desktop.LevelLow, AvailableBytes: 2.1e9, TotalBytes: 16.7e9},
		Disk:   desktop.DiskInfo{Level: desktop.LevelOK, FreeBytes: 81e9, TotalBytes: 250e9},
	}
	var h struct {
		MaxDesktops int              `json:"maxDesktops"`
		Desktops    map[string]int64 `json:"desktops"`
		Checks      map[string]check `json:"checks"`
	}
	rr := doJSON(t, s, http.MethodGet, "/health", "")
	if err := json.Unmarshal(rr.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if h.MaxDesktops != 6 || h.Desktops["count"] != 2 || h.Desktops["asleep"] != 1 || h.Desktops["memoryBytes"] != 3e9 {
		t.Errorf("health %s", rr.Body)
	}
	if c := h.Checks["memory"]; c.OK || !strings.HasPrefix(c.Detail, "2.1 GB of 16.7 GB available (12%)") {
		t.Errorf("memory %+v", c)
	}
	if c := h.Checks["disk"]; !c.OK || c.Detail != "81 GB free" {
		t.Errorf("disk %+v", c)
	}
	// Read live, not from the cache.
	p.mu.Lock()
	p.res.Disk.Level = desktop.LevelCritical
	p.mu.Unlock()
	rr = doJSON(t, s, http.MethodGet, "/health", "")
	_ = json.Unmarshal(rr.Body.Bytes(), &h)
	if h.Checks["disk"].OK {
		t.Error("disk check cached")
	}
}

func TestDesktopAdmin(t *testing.T) {
	p := &scripted{}
	s, _ := dispatchServer(t, p, 1, "")
	a, _ := s.store.Create("a")
	if id, err := s.ResolveSession(a.SessionID[:6]); err != nil || id != a.SessionID {
		t.Errorf("prefix: %q %v", id, err)
	}
	if _, err := s.ResolveSession(a.SessionID[:3]); err == nil {
		t.Error("3-character prefix accepted")
	}
	// Two sessions sharing a prefix: ambiguous.
	b, _ := s.store.Create("b")
	common := 0
	for common < len(a.SessionID) && a.SessionID[common] == b.SessionID[common] {
		common++
	}
	if common >= 4 {
		if _, err := s.ResolveSession(a.SessionID[:common]); err == nil {
			t.Error("ambiguous prefix accepted")
		}
	}
	p.list = []desktop.Info{{SessionID: a.SessionID, State: desktop.StateRunning}, {SessionID: "0rphan0000", State: desktop.StateAsleep}, {SessionID: "x", State: desktop.StateFailed}}
	n, err := s.CloseAllDesktops(context.Background(), desktop.CloseStop)
	if err != nil || n != 2 {
		t.Errorf("CloseAll %d %v", n, err)
	}
	if !slices.Contains(p.Calls(), "close "+a.SessionID+" stop") || !slices.Contains(p.Calls(), "close 0rphan0000 stop") {
		t.Errorf("calls %q", p.Calls())
	}
	if !strings.HasSuffix(s.DesktopsDir(), "/desktops") {
		t.Error(s.DesktopsDir())
	}
}
