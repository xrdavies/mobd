package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xrdavies/mobd/internal/artifact"
	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/pairing"
	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/settings"
	"github.com/xrdavies/mobd/internal/stream"
	"github.com/xrdavies/mobd/internal/task"
)

type Server struct {
	pairing *pairing.Store
	cfg     config.Config
	store   *task.Store
	runners *runner.Registry
	// settings is settings.json: the agents' and conversations' runtime settings.
	settings *settings.Store
	// supported are all the agents mobd knows, enabled or not, for the installation check.
	supported []runner.Runner

	hub       *hub
	artifacts *artifact.Store
	health    healthCache
	files     *fileService

	streams *stream.Manager
	// desktops gives each session its desktop: the shared one, or its own on Linux.
	desktops desktop.Provider

	// self is how `mobd mcp` reaches this server; empty uses cfg.SelfURL().
	self string

	// qmu orders question resolution with waking the agent waiting for it.
	qmu     sync.Mutex
	waiters map[string]chan struct{}

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	// tokens authenticate `mobd mcp` calls from a dispatched turn.
	tokens map[string]string
	// reservations hold each dispatched turn's place on its desktop until the turn ends.
	reservations map[string]desktop.Reservation
	// starting marks dispatched turns whose desktop is still starting.
	starting map[string]bool
	closing  bool
	runs     sync.WaitGroup
	done     chan struct{}
}

// New serves the given runners; cfg.DefaultRunner (or the first runner) starts
// new conversations. Callers validate DefaultRunner before calling New. desktops gives each
// session its desktop; nil means this OS's one shared desktop.
func New(cfg config.Config, store *task.Store, auth *pairing.Store, desktops desktop.Provider, runners ...runner.Runner) *Server {
	if auth == nil {
		panic("pairing store is required")
	}
	def := ""
	if len(runners) > 0 {
		def = runners[0].Info().Name
	}
	reg, err := runner.NewRegistry(def, runners...)
	if err != nil {
		panic(err)
	}
	s := &Server{
		cfg: cfg, store: store, pairing: auth, runners: reg, hub: newHub(),
		settings:  settings.Open(filepath.Join(dataDir(cfg), "settings.json")),
		artifacts: artifact.New(filepath.Join(dataDir(cfg), "artifacts")),
		cancels:   make(map[string]context.CancelFunc), done: make(chan struct{}),
		tokens: make(map[string]string), waiters: make(map[string]chan struct{}),
		reservations: make(map[string]desktop.Reservation), starting: make(map[string]bool),
	}
	s.files = newFileService(filesDir(cfg))
	// settings.json is created on the first start and gets an entry for each newly enabled agent.
	var enabled []string
	for _, r := range runners {
		enabled = append(enabled, r.Info().Name)
	}
	if err := s.settings.Ensure(enabled, func(name string) settings.Agent {
		rn, _ := reg.Get(name)
		return seedAgent(rn.Info())
	}); err != nil {
		log.Printf("settings: %v", err)
	}
	s.applyDefaultAgent(s.settings.Get())
	if desktops == nil {
		desktops = desktop.Shared(desktop.Current())
	}
	s.desktops = desktops
	streams, err := stream.NewManager(stream.Config{
		Source: func(sessionID string) (stream.Source, error) {
			if desktops.PerSession() {
				// Each conversation has its own desktop: say which, and it must exist.
				if sessionID == "" {
					return nil, errSessionRequired
				}
				if _, ok := store.SessionTitle(sessionID); !ok {
					return nil, errStreamSession
				}
			}
			d, err := desktops.Get(sessionID)
			if err != nil {
				return nil, err
			}
			return stream.FromDesktop(d.Source()), nil
		},
		KeepAwakeAfter: 30 * time.Second,
		Attach: func(parent context.Context, client string) (context.Context, func(), error) {
			return auth.Attach(parent, client, http.MethodPost, "/stream")
		},
		UDP: cfg.StreamUDP,
	})
	if err != nil {
		panic(err)
	}
	s.streams = streams
	store.SetNotifier(s.onChange)
	// A desktop closing, failing or finishing its start, and memory or disk changing, can let
	// a waiting turn through.
	desktops.Subscribe(s.onDesktopEvent)
	return s
}

// filesDir is the shared folder; configs built in tests keep it inside their data directory.
func filesDir(cfg config.Config) string {
	if cfg.FilesDir != "" {
		return cfg.FilesDir
	}
	return filepath.Join(dataDir(cfg), "files")
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
	mux.HandleFunc("GET /settings", s.handleGetSettings)
	mux.HandleFunc("PUT /settings", s.handlePutSettings)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /screen", s.handleScreen)
	mux.HandleFunc("GET /desktops", s.handleListDesktops)
	mux.HandleFunc("POST /desktops/{id}", s.handleOpenDesktop)
	mux.HandleFunc("DELETE /desktops/{id}", s.handleCloseDesktop)
	mux.HandleFunc("POST /desktops/{id}/seed", s.handleSeedDesktop)
	mux.HandleFunc("POST /uploads", s.handleUpload)
	mux.HandleFunc("GET /artifacts/{id}", s.handleArtifact)
	mux.HandleFunc("GET /files", s.files.list)
	mux.HandleFunc("GET /files/{path...}", s.files.download)
	mux.HandleFunc("POST /files/{path...}", s.files.rename)
	mux.HandleFunc("DELETE /files/{path...}", s.files.remove)
	mux.HandleFunc("POST /file-folders", s.files.createFolder)
	mux.HandleFunc("POST /file-uploads/{id}/complete", s.files.complete)
	mux.Handle("/file-uploads/", http.HandlerFunc(s.files.upload))
	mux.HandleFunc("POST /stream", s.handleStreamOpen)
	mux.HandleFunc("DELETE /stream/{id}", s.handleStreamClose)
	mux.HandleFunc("GET /stream/ws", s.handleStreamWS)
	mux.HandleFunc("POST /tasks/{id}/questions/{qid}/answer", s.handleAnswer)
	mux.HandleFunc("POST /tasks/{id}/steer", s.handleSteer)
	public := http.NewServeMux()
	public.HandleFunc("POST /pair", s.handlePair)
	public.HandleFunc("GET /pair", s.handlePairInfo)
	// A probe code from the management socket: `mobd remote` checks an address reaches this mobd.
	public.HandleFunc("GET /probe/{code}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if s.pairing.Probe(r.PathValue("code")) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	})
	public.HandleFunc("POST /internal/tasks/{id}/ask", s.handleAsk)
	public.HandleFunc("POST /internal/tasks/{id}/desktop/{op}", s.handleInternalDesktop)
	public.Handle("/", s.withAuth(mux))
	return s.withCORS(withGzip(public))
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
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, If-None-Match, If-Match, If-Range, Range, Tus-Resumable, Upload-Length, Upload-Offset, Upload-Metadata, X-HTTP-Method-Override")
			w.Header().Set("Access-Control-Expose-Headers", "ETag, Location, Tus-Resumable, Tus-Version, Tus-Extension, Tus-Max-Size, Upload-Offset, Upload-Length, Content-Length, Content-Range, Content-Disposition, Accept-Ranges, X-Content-SHA256")
		}
		if r.Method == http.MethodOptions {
			if origin == "" {
				if strings.HasPrefix(r.URL.Path, "/file-uploads/") {
					s.files.options(w)
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if _, ok := allowed[origin]; !ok {
				writeErr(w, http.StatusForbidden, "origin not allowed")
				return
			}
			if strings.HasPrefix(r.URL.Path, "/file-uploads/") {
				s.files.options(w)
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
	t, err := s.store.CreateTurnWith(req.SessionID, req.Prompt, opts)
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
	// The response says why the new turn waits, as set by scheduling.
	if fresh, err := s.store.Get(t.ID); err == nil && fresh.Status == task.StatusQueued {
		t.QueueReason = fresh.QueueReason
	}
	writeJSON(w, http.StatusCreated, t)
}

// scheduleLocked starts queued turns in FIFO order while slots are free and their desktop can
// be had; a turn held by its desktop (limit, memory, disk) lets later turns of other sessions
// pass. Every waiting turn gets its queue reason. A dispatched task has a cancel func even
// before it reports running. Caller holds mu.
func (s *Server) scheduleLocked() {
	limit := max(s.cfg.MaxRunning, 1)
	reasons := map[string]string{}
	for _, t := range s.store.QueuedTasks() {
		if s.closing {
			break
		}
		if s.cancels[t.ID] != nil {
			if s.starting[t.ID] {
				reasons[t.ID] = string(desktop.WaitStarting)
			}
			continue
		}
		if len(s.cancels) >= limit {
			reasons[t.ID] = string(desktop.WaitSlots)
			continue
		}
		r, why, ok := s.desktops.Reserve(t.SessionID)
		if !ok {
			reasons[t.ID] = string(why)
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		s.cancels[t.ID], s.tokens[t.ID], s.reservations[t.ID] = cancel, newToken(), r
		if s.desktops.PerSession() {
			s.starting[t.ID] = true
			reasons[t.ID] = string(desktop.WaitStarting)
		}
		s.runs.Add(1)
		go s.runTask(ctx, t.ID)
	}
	s.store.SetQueueReasons(reasons)
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
	if patch.Model != nil || patch.Effort != nil {
		if problem, code := s.checkSessionSettings(r.PathValue("id"), patch); code != 0 {
			writeErr(w, code, problem)
			return
		}
	}
	session, err := s.store.UpdateSession(r.PathValue("id"), patch)
	if err == nil && patch.Archived != nil && *patch.Archived && s.desktops.PerSession() {
		// An archived conversation keeps no desktop; its running turn stops first.
		s.closeDesktop(session.ID, session.ActiveTaskID, desktop.CloseArchived)
	}
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
	id := r.PathValue("id")
	artifacts, err := s.store.DeleteSession(id)
	if err == nil {
		s.artifacts.Delete(artifacts...)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := s.desktops.Remove(ctx, id); err != nil {
				log.Printf("remove desktop of deleted session %s: %v", id, err)
			}
		}()
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

// handleGetSession returns a conversation. With ?since=REV (the rev of an earlier answer) it
// returns only the turns changed after it, with their messages, steps and questions; "full"
// says whether the client should replace what it holds or merge the turns into it.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	var since int64
	if raw := r.URL.Query().Get("since"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "since must be a revision number")
			return
		}
		since = n
	}
	session, tasks, activities, questions, full, err := s.store.SessionSince(r.PathValue("id"), since)
	if err != nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session": session, "tasks": tasks, "activities": activities, "questions": questions,
		"rev": session.Rev, "full": full,
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
	t, err := s.cancelLocked(id)
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

// cancelLocked stops a dispatched turn, or finishes a queued one at once. Caller holds mu.
func (s *Server) cancelLocked(id string) (*task.Task, error) {
	t, err := s.store.CanCancel(id)
	if err != nil {
		return nil, err
	}
	if cancel := s.cancels[id]; cancel != nil {
		cancel()
		return t, nil
	}
	// Still waiting for a slot or its desktop: nothing to stop, finish it now.
	return s.store.UpdateStatus(id, task.StatusCancelled, "cancelled")
}

// closeDesktop cancels the session's active turn, if any, then closes its desktop in the
// background (the owner's decision: closing never waits for a turn to finish).
func (s *Server) closeDesktop(sessionID, activeTask string, reason desktop.CloseReason) {
	s.cancelSessionTurn(activeTask)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.desktops.Close(ctx, sessionID, reason); err != nil {
			log.Printf("close desktop of session %s: %v", sessionID, err)
		}
	}()
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
		if r := s.reservations[id]; r != nil {
			r.Release()
		}
		delete(s.cancels, id)
		delete(s.tokens, id)
		delete(s.reservations, id)
		delete(s.starting, id)
		s.scheduleLocked()
		s.mu.Unlock()
		s.runs.Done()
	}()
	t, err := s.store.Get(id)
	var env desktop.TurnEnv
	if err == nil {
		// The turn stays queued ("desktopStarting") until its desktop runs.
		env, err = s.turnDesktop(ctx, t)
	}
	if err == nil {
		t, err = s.store.UpdateStatus(id, task.StatusRunning, "")
	}
	if err == nil {
		rn, ok := s.runners.Get(t.Runner)
		if !ok {
			err = fmt.Errorf("runner %q is not enabled on this mobd", t.Runner)
		} else {
			var opts runner.Options
			var done func()
			if opts, done, err = s.runOptions(t, env); err == nil {
				err = rn.Start(ctx, t, opts, func(ev task.Event) error { return s.store.AppendEvent(id, ev) })
				done()
			}
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

// turnDesktop starts or wakes the turn's desktop and returns how the runner reaches it.
func (s *Server) turnDesktop(ctx context.Context, t *task.Task) (desktop.TurnEnv, error) {
	s.mu.Lock()
	r := s.reservations[t.ID]
	s.mu.Unlock()
	d, err := s.desktops.Ensure(ctx, t.SessionID, "turn")
	s.mu.Lock()
	delete(s.starting, t.ID)
	s.mu.Unlock()
	if err != nil {
		return desktop.TurnEnv{}, err
	}
	return r.Turn(d)
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
