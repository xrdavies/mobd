package api

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/xrdavies/mobd/internal/task"
)

type historyAnswer struct {
	Session task.Session `json:"session"`
	Tasks   []task.Task  `json:"tasks"`
	Rev     int64        `json:"rev"`
	HasMore bool         `json:"hasMore"`
	Before  string       `json:"before"`
	Reset   bool         `json:"reset"`
}

func getHistory(t *testing.T, s *Server, path string) (int, historyAnswer) {
	t.Helper()
	rr := httptest.NewRecorder()
	authenticatedHandler(t, s).ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
	var a historyAnswer
	_ = json.Unmarshal(rr.Body.Bytes(), &a)
	return rr.Code, a
}

func finishedTurns(t *testing.T, store *task.Store, n int) string {
	t.Helper()
	var sid string
	for i := 0; i < n; i++ {
		tk, err := store.CreateTurn(sid, "turn")
		if err != nil {
			t.Fatal(err)
		}
		sid = tk.SessionID
		_, _ = store.UpdateStatus(tk.ID, task.StatusRunning, "")
		_, _ = store.UpdateStatus(tk.ID, task.StatusSucceeded, "")
	}
	return sid
}

func TestSessionHistoryPagesAndSync(t *testing.T) {
	s := testServer(t, "")
	sid := finishedTurns(t, s.store, 5)
	other := finishedTurns(t, s.store, 2)
	base := "/sessions/" + sid + "/history"

	code, first := getHistory(t, s, base+"?limit=2")
	if code != 200 || len(first.Tasks) != 2 || first.Tasks[0].Seq != 4 || !first.HasMore || first.Before == "" || first.Rev == 0 || first.Session.Turns != 5 {
		t.Fatalf("newest: %d %+v", code, first)
	}
	code, older := getHistory(t, s, base+"?limit=2&before="+first.Before)
	if code != 200 || len(older.Tasks) != 2 || older.Tasks[0].Seq != 2 || older.Tasks[1].Seq != 3 {
		t.Fatalf("older: %d %+v", code, older)
	}
	code, last := getHistory(t, s, base+"?before="+older.Before)
	if code != 200 || len(last.Tasks) != 1 || last.HasMore || last.Before != "" {
		t.Fatalf("oldest: %d %+v", code, last)
	}
	code, def := getHistory(t, s, base)
	if code != 200 || len(def.Tasks) != 5 {
		t.Fatalf("default limit: %d %d", code, len(def.Tasks))
	}

	if code, a := getHistory(t, s, base+"?since="+itoa(first.Rev)); code != 200 || a.Reset || len(a.Tasks) != 0 {
		t.Fatalf("since unchanged: %d %+v", code, a)
	}
	if code, a := getHistory(t, s, base+"?since=0"); code != 200 || !a.Reset || a.Session.ID != sid {
		t.Fatalf("since 0 resets: %d %+v", code, a)
	}

	_, foreign := getHistory(t, s, "/sessions/"+other+"/history?limit=1")
	for _, path := range []string{
		base + "?before=" + foreign.Before, base + "?before=not-a-cursor", base + "?limit=0", base + "?limit=51",
		base + "?limit=x", base + "?since=-1", base + "?since=x", base + "?since=1&before=" + first.Before,
	} {
		if code, _ := getHistory(t, s, path); code != 400 {
			t.Errorf("%s: %d, want 400", path, code)
		}
	}
	if code, _ := getHistory(t, s, "/sessions/missing/history"); code != 404 {
		t.Errorf("missing session: %d", code)
	}
	if !slices.Contains(s.features(), "sessionHistory") {
		t.Error("sessionHistory not advertised")
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
