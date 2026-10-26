package api

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestJSONAndEventStreamsAreCompressed(t *testing.T) {
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
	// The finished turn's event stream replays and ends, compressed.
	rr := do("/tasks/"+tk.ID+"/events", true)
	if rr.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(rr.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("events: %q %q", rr.Header().Get("Content-Encoding"), rr.Header().Get("Content-Type"))
	}
	zr, err = gzip.NewReader(rr.Body)
	if err != nil {
		t.Fatal(err)
	}
	if events, _ := io.ReadAll(zr); !strings.Contains(string(events), "succeeded") {
		t.Fatalf("events body: %s", events)
	}
	if rr := do("/tasks/"+tk.ID+"/events", false); rr.Header().Get("Content-Encoding") != "" || !strings.Contains(rr.Body.String(), "succeeded") {
		t.Fatalf("plain events: %q %s", rr.Header().Get("Content-Encoding"), rr.Body.String())
	}
}

// A compressed stream must deliver each event when it is sent, not when the stream ends, and the
// repeated "reply so far" must cost little.
func TestCompressedEventStreamDeliversEachEventAtOnce(t *testing.T) {
	s := testServer(t, "")
	credential := pairedClient(t, s)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	tk, _ := s.store.Create("write a long answer")
	_, _ = s.store.UpdateStatus(tk.ID, task.StatusRunning, "")

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/tasks/"+tk.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+credential.Token)
	req.Header.Set("Accept-Encoding", "gzip")
	// Read the raw bytes, so the test sees the encoding and decodes incrementally itself.
	res, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("encoding %q", res.Header.Get("Content-Encoding"))
	}
	counted := &countingReader{r: res.Body}
	zr, err := gzip.NewReader(counted)
	if err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewReader(zr)
	next := func() string {
		got := make(chan string, 1)
		go func() {
			var ev strings.Builder
			for {
				line, err := lines.ReadString('\n')
				ev.WriteString(line)
				if err != nil || line == "\n" {
					got <- ev.String()
					return
				}
			}
		}()
		select {
		case ev := <-got:
			return ev
		case <-time.After(3 * time.Second):
			t.Fatal("event not delivered while the stream stays open")
			return ""
		}
	}
	for !strings.Contains(next(), "status\":\"running") { // the statuses replayed on connect
	}

	reply := strings.Repeat("The store test fails because the late question clears the flag. ", 160) // ~10 KB
	var plain int
	for i := 1; i <= 50; i++ {
		text := reply[:len(reply)*i/50]
		if err := s.store.AppendEvent(tk.ID, task.Event{Type: "message", MessageID: "m1", Message: text}); err != nil {
			t.Fatal(err)
		}
		ev := next()
		plain += len(ev)
		if !strings.Contains(ev, "event: message") || !strings.Contains(ev, text[len(text)-20:]) {
			t.Fatalf("event %d: %.200s", i, ev)
		}
	}
	t.Logf("%d KB of events, %d KB on the wire", plain/1024, counted.n.Load()/1024)
	if sent := counted.n.Load(); sent*10 > int64(plain) {
		t.Fatalf("%d bytes on the wire for %d bytes of events: repeats are not compressed", sent, plain)
	}
}

type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}
