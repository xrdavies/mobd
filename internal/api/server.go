package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xrdavies/mobd/internal/artifact"
	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/pairing"
	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/stream"
	"github.com/xrdavies/mobd/internal/task"
)

type Server struct {
	pairing *pairing.Store
	cfg     config.Config
	store   *task.Store
	runners *runner.Registry

	hub       *hub
	artifacts *artifact.Store
	health    healthCache

	vncSessions atomic.Int64
	streams     *stream.Manager

	// self is how `mobd mcp` reaches this server; empty uses cfg.SelfURL().
	self string

	// qmu orders question resolution with waking the agent waiting for it.
	qmu     sync.Mutex
	waiters map[string]chan struct{}

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	// tokens authenticate `mobd mcp` calls from a dispatched turn.
	tokens  map[string]string
	closing bool
	runs    sync.WaitGroup
	done    chan struct{}
}

// New serves the given runners; cfg.DefaultRunner (or the first runner) starts
// new conversations. Callers validate DefaultRunner before calling New.
func New(cfg config.Config, store *task.Store, auth *pairing.Store, runners ...runner.Runner) *Server {
	if auth == nil {
		panic("pairing store is required")
	}
	def := cfg.DefaultRunner
	known := false
	for _, r := range runners {
		known = known || r.Info().Name == def
	}
	if !known && len(runners) > 0 {
		def = runners[0].Info().Name
	}
	reg, err := runner.NewRegistry(def, runners...)
	if err != nil {
		panic(err)
	}
	s := &Server{
		cfg: cfg, store: store, pairing: auth, runners: reg, hub: newHub(),
		artifacts: artifact.New(filepath.Join(dataDir(cfg), "artifacts")),
		cancels:   make(map[string]context.CancelFunc), done: make(chan struct{}),
		tokens: make(map[string]string), waiters: make(map[string]chan struct{}),
	}
	streams, err := stream.NewManager(stream.Config{
		Start: desktop.StartStream, KeepAwake: desktop.KeepAwake,
		Display: func() (desktop.Display, error) {
			d, err := desktop.Current().Displays()
			if err != nil || len(d) == 0 {
				return desktop.Display{}, cmp.Or(err, errors.New("no display"))
			}
			return d[0], nil
		},
		Attach: func(parent context.Context, client string) (context.Context, func(), error) {
			return auth.Attach(parent, client, http.MethodPost, "/stream")
		},
		NewInput: func() stream.Input { return desktop.NewInput() },
	})
	if err != nil {
		panic(err)
	}
	s.streams = streams
	store.SetNotifier(s.onChange)
	return s
}

// dataDir falls back to a private temp directory for configs built in tests.
func dataDir(cfg config.Config) string {
	if cfg.DataDir != "" {
		return cfg.DataDir
	}
	return filepath.Join(os.TempDir(), "mobd-data")
}

// onChange runs under the store lock; the hub never calls back into the store.
func (s *Server) onChange(c task.Change) {
	if c.DeletedSession != "" {
		s.hub.publish("session.deleted", map[string]string{"id": c.DeletedSession})
	}
	if c.Session != nil {
		s.hub.publish("session", c.Session)
	}
	if c.Task != nil {
		s.hub.publish("task", c.Task)
		s.hub.publish("queue", map[string]any{"running": c.Running, "queued": c.Queued})
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /sessions", s.handleListSessions)
	mux.HandleFunc("GET /sessions/{id}", s.handleGetSession)
	mux.HandleFunc("PATCH /sessions/{id}", s.handlePatchSession)
	mux.HandleFunc("DELETE /sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("POST /tasks", s.handleCreateTask)
	mux.HandleFunc("GET /tasks", s.handleListTasks)
	mux.HandleFunc("GET /tasks/{id}", s.handleGetTask)
	mux.HandleFunc("GET /tasks/{id}/events", s.handleTaskEvents)
	mux.HandleFunc("POST /tasks/{id}/cancel", s.handleCancelTask)
	mux.HandleFunc("GET /queue", s.handleQueue)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /screen", s.handleScreen)
	mux.HandleFunc("POST /uploads", s.handleUpload)
	mux.HandleFunc("GET /artifacts/{id}", s.handleArtifact)
	mux.HandleFunc("GET /vnc", s.handleVNC)
	mux.HandleFunc("POST /vnc/ticket", s.handleVNCTicket)
	mux.HandleFunc("POST /stream", s.handleStreamOpen)
	mux.HandleFunc("DELETE /stream/{id}", s.handleStreamClose)
	mux.HandleFunc("GET /stream/ws", s.handleStreamWS)
	mux.HandleFunc("POST /tasks/{id}/questions/{qid}/answer", s.handleAnswer)
	mux.HandleFunc("POST /tasks/{id}/steer", s.handleSteer)
	public := http.NewServeMux()
	public.HandleFunc("POST /pair", s.handlePair)
	public.HandleFunc("GET /pair", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		writeJSON(w, http.StatusOK, map[string]string{"service": "mobd", "message": "Open or paste the complete invitation link in a pairing-capable mob client."})
	})
	public.HandleFunc("POST /internal/tasks/{id}/ask", s.handleAsk)
	public.Handle("/", s.withAuth(mux))
	return s.withCORS(public)
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(s.cfg.CORSOrigins))
	for _, origin := range s.cfg.CORSOrigins {
		allowed[origin] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		if _, ok := allowed[origin]; ok {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, If-None-Match")
			w.Header().Set("Access-Control-Expose-Headers", "ETag")
		}
		if r.Method == http.MethodOptions {
			if origin == "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if _, ok := allowed[origin]; !ok {
				writeErr(w, http.StatusForbidden, "origin not allowed")
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req task.CreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body (maximum 1 MiB)")
		return
	}
	opts, problem, code := s.turnOptions(req)
	if problem != "" {
		writeErr(w, code, problem)
		return
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		writeErr(w, http.StatusServiceUnavailable, "mobd is stopping")
		return
	}
	t, err := s.store.CreateTurnWith(req.SessionID, req.Prompt, s.cfg.VNCViewURL, opts)
	if err != nil {
		s.mu.Unlock()
		code := http.StatusInternalServerError
		switch {
		case errors.Is(err, task.ErrInvalidPrompt):
			code = http.StatusBadRequest
		case errors.Is(err, task.ErrNotFound):
			code = http.StatusNotFound
		case errors.Is(err, task.ErrSessionBusy), errors.Is(err, task.ErrRunnerMismatch):
			code = http.StatusConflict
		}
		writeErr(w, code, err.Error())
		return
	}
	s.scheduleLocked()
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, t)
}

// scheduleLocked starts queued turns in FIFO order while desktop slots are free.
// A dispatched task has a cancel func even before it reports running. Caller holds mu.
func (s *Server) scheduleLocked() {
	limit := max(s.cfg.MaxRunning, 1)
	for !s.closing && len(s.cancels) < limit {
		next := s.store.NextQueued(func(id string) bool { return s.cancels[id] != nil })
		if next == nil {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		s.cancels[next.ID] = cancel
		s.tokens[next.ID] = newToken()
		s.runs.Add(1)
		go s.runTask(ctx, next.ID)
	}
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	running, queued := s.store.Queue()
	writeJSON(w, http.StatusOK, map[string]any{"running": running, "queued": queued})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	var archived *bool
	switch r.URL.Query().Get("archived") {
	case "", "false":
		archived = new(bool)
	case "true":
		archived = new(bool)
		*archived = true
	case "all":
	default:
		writeErr(w, http.StatusBadRequest, "archived must be true, false or all")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.store.ListSessions(archived)})
}

func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	var patch task.SessionPatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	session, err := s.store.UpdateSession(r.PathValue("id"), patch)
	switch {
	case errors.Is(err, task.ErrNotFound):
		writeErr(w, http.StatusNotFound, "session not found")
	case errors.Is(err, task.ErrInvalidTitle):
		writeErr(w, http.StatusBadRequest, err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, session)
	}
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	artifacts, err := s.store.DeleteSession(r.PathValue("id"))
	if err == nil {
		s.artifacts.Delete(artifacts...)
	}
	switch {
	case errors.Is(err, task.ErrNotFound):
		writeErr(w, http.StatusNotFound, "session not found")
	case errors.Is(err, task.ErrSessionBusy):
		writeErr(w, http.StatusConflict, err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	session, tasks, activities, err := s.store.Session(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session": session, "tasks": tasks, "activities": activities, "questions": s.store.Questions(session.ID),
	})
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := task.TaskFilter{SessionID: q.Get("sessionId"), Status: task.Status(q.Get("status")), Limit: 100}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			writeErr(w, http.StatusBadRequest, "limit must be 1-500")
			return
		}
		f.Limit = n
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": s.store.ListTasks(f)})
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	t, err := s.store.CanCancel(id)
	if err == nil {
		if cancel := s.cancels[id]; cancel != nil {
			cancel()
		} else {
			// Still waiting for the desktop: nothing to stop, finish it now.
			t, err = s.store.UpdateStatus(id, task.StatusCancelled, "cancelled")
		}
	}
	s.mu.Unlock()
	if err != nil {
		code := http.StatusConflict
		if errors.Is(err, task.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, code, err.Error())
		return
	}
	// Keep the session busy until the process and its output readers have stopped.
	writeJSON(w, http.StatusAccepted, t)
}

func (s *Server) handleTaskEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.Get(id); err != nil {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = r.URL.Query().Get("after")
	}
	var after int64
	if cursor != "" {
		var err error
		after, err = strconv.ParseInt(cursor, 10, 64)
		if err != nil || after < 0 {
			writeErr(w, http.StatusBadRequest, "invalid event cursor")
			return
		}
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		past, terminal, changed, err := s.store.Watch(id, after)
		if err != nil {
			return
		}
		for _, ev := range past {
			if err := writeSSE(w, ev); err != nil {
				return
			}
			after = ev.ID
			flusher.Flush()
		}
		if terminal {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-changed:
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleEvents streams global summaries. Without a cursor it starts live, so
// clients should subscribe before fetching snapshots.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	after := s.hub.cursor()
	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = r.URL.Query().Get("after")
	}
	if cursor != "" {
		var err error
		after, err = strconv.ParseInt(cursor, 10, 64)
		if err != nil || after < 0 {
			writeErr(w, http.StatusBadRequest, "invalid event cursor")
			return
		}
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		events, reset, next, changed := s.hub.since(after)
		if reset {
			if _, err := fmt.Fprintf(w, "id: %d\nevent: reset\ndata: {}\n\n", next); err != nil {
				return
			}
			flusher.Flush()
		}
		after = next
		for _, ev := range events {
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, ev.Data); err != nil {
				return
			}
			after = ev.ID
		}
		if len(events) > 0 {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-changed:
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) runTask(ctx context.Context, id string) {
	defer func() {
		s.mu.Lock()
		s.cancels[id]()
		delete(s.cancels, id)
		delete(s.tokens, id)
		s.scheduleLocked()
		s.mu.Unlock()
		s.runs.Done()
	}()
	t, err := s.store.UpdateStatus(id, task.StatusRunning, "")
	if err == nil {
		rn, ok := s.runners.Get(t.Runner)
		if !ok {
			err = fmt.Errorf("runner %q is not enabled on this mobd", t.Runner)
		} else {
			err = rn.Start(ctx, t, s.runOptions(t), func(ev task.Event) error { return s.store.AppendEvent(id, ev) })
		}
	}
	if t != nil {
		s.cancelQuestions(t)
	}
	status, message := task.StatusSucceeded, ""
	if ctx.Err() != nil {
		status, message = task.StatusCancelled, "cancelled"
	} else if err != nil {
		status, message = task.StatusFailed, err.Error()
	}
	if _, err := s.store.UpdateStatus(id, status, message); err != nil {
		log.Printf("finish task %s: %v", id, err)
		if emitErr := s.store.AppendEvent(id, task.Event{Type: "error", Message: err.Error()}); emitErr != nil {
			log.Printf("report task %s failure: %v", id, emitErr)
		}
	}
}

// Close cancels active turns, waits for their final state to be saved, and
// ends open event streams.
func (s *Server) Close() {
	s.mu.Lock()
	if !s.closing {
		close(s.done)
	}
	s.closing = true
	for _, cancel := range s.cancels {
		cancel()
	}
	s.mu.Unlock()
	s.streams.Shutdown()
	s.runs.Wait()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeSSE(w http.ResponseWriter, ev task.Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, b)
	return err
}
