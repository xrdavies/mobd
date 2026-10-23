package api

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/xrdavies/mobd/internal/task"
)

func TestSessionSinceOverHTTP(t *testing.T) {
	s := testServer(t, "")
	tk, _ := s.store.Create("first")
	_, _ = s.store.UpdateStatus(tk.ID, task.StatusSucceeded, "")
	type answer struct {
		Rev   int64
		Full  bool
		Tasks []task.Task
	}
	get := func(query string) (int, answer) {
		rr := doJSON(t, s, http.MethodGet, "/sessions/"+tk.SessionID+query, "")
		var a answer
		_ = json.Unmarshal(rr.Body.Bytes(), &a)
		return rr.Code, a
	}
	code, all := get("")
	if code != http.StatusOK || !all.Full || all.Rev == 0 || len(all.Tasks) != 1 {
		t.Fatalf("full: %d %+v", code, all)
	}
	if code, same := get("?since=" + strconv.FormatInt(all.Rev, 10)); code != http.StatusOK || same.Full || len(same.Tasks) != 0 || same.Rev != all.Rev {
		t.Fatalf("unchanged: %d %+v", code, same)
	}
	if code, _ := get("?since=abc"); code != http.StatusBadRequest {
		t.Fatalf("bad since: %d", code)
	}
}

func TestJSONIsCompressedButEventStreamsAreNot(t *testing.T) {
	s := testServer(t, "")
	tk, _ := s.store.Create(strings.Repeat("a long prompt ", 200))
	_, _ = s.store.UpdateStatus(tk.ID, task.StatusSucceeded, "")
	h := authenticatedHandler(t, s)
	do := func(path string, gz bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if gz {
			r.Header.Set("Accept-Encoding", "gzip")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	plain := do("/sessions/"+tk.SessionID, false)
	zipped := do("/sessions/"+tk.SessionID, true)
	if plain.Header().Get("Content-Encoding") != "" || zipped.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("encodings: plain %q gzip %q", plain.Header().Get("Content-Encoding"), zipped.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(zipped.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if string(body) != plain.Body.String() || zipped.Body.Len() >= plain.Body.Len() {
		t.Fatalf("gzip body %d bytes, plain %d; equal=%v", zipped.Body.Len(), plain.Body.Len(), string(body) == plain.Body.String())
	}
	// Small answers stay plain.
	if rr := do("/sessions/missing", true); rr.Header().Get("Content-Encoding") != "" || !strings.Contains(rr.Body.String(), "not found") {
		t.Fatalf("small: %q %s", rr.Header().Get("Content-Encoding"), rr.Body.String())
	}
	// The finished turn's event stream replays and ends; it must not be compressed.
	if rr := do("/tasks/"+tk.ID+"/events", true); rr.Header().Get("Content-Encoding") != "" || !strings.Contains(rr.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(rr.Body.String(), "succeeded") {
		t.Fatalf("events: %q %q %s", rr.Header().Get("Content-Encoding"), rr.Header().Get("Content-Type"), rr.Body.String())
	}
}
