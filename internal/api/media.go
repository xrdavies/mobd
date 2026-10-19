package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xrdavies/mobd/internal/artifact"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/task"
)

const maxUpload = 20 << 20

var validModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// snapshot is swapped in tests.
var snapshot = func(ctx context.Context, d desktop.Desktop, opts desktop.SnapshotOptions) (desktop.Image, desktop.DesktopState, error) {
	return d.Snapshot(ctx, opts)
}

func (s *Server) handleScreen(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	format := q.Get("format")
	if format == "" {
		format = "jpeg"
	}
	maxWidth, quality := 0, 80
	var err error
	if raw := q.Get("maxWidth"); raw != "" {
		if maxWidth, err = strconv.Atoi(raw); err != nil || maxWidth < 16 || maxWidth > 8192 {
			writeErr(w, http.StatusBadRequest, "maxWidth must be 16-8192")
			return
		}
	}
	if raw := q.Get("quality"); raw != "" {
		if quality, err = strconv.Atoi(raw); err != nil || quality < 1 || quality > 100 {
			writeErr(w, http.StatusBadRequest, "quality must be 1-100")
			return
		}
	}
	if format != "jpeg" && format != "png" {
		writeErr(w, http.StatusBadRequest, "format must be jpeg or png")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	d, err := s.desktops.Get(q.Get("sessionId"))
	if errors.Is(err, desktop.ErrNoDesktop) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	img, state, err := snapshot(ctx, d, desktop.SnapshotOptions{MaxWidth: maxWidth, Format: format, Quality: quality})
	if state != "" && state != desktop.StateRunning {
		// An asleep conversation desktop answers with its last frame and is not woken.
		w.Header().Set("X-Mob-Desktop-State", string(state))
	}
	if errors.Is(err, desktop.ErrUnsupported) {
		writeErr(w, http.StatusNotImplemented, err.Error())
		return
	}
	if errors.Is(err, desktop.ErrNoDesktop) {
		// Starting, or asleep without a frame from before.
		writeErrCode(w, http.StatusConflict, "no_desktop", err.Error())
		return
	}
	if err != nil {
		// Includes ErrScreenUnavailable: its message says the computer is locked or asleep.
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	// Clients polling an unchanged screen send If-None-Match and skip the download.
	sum := sha256.Sum256(img.Data)
	etag := `"` + hex.EncodeToString(sum[:12]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-store")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", img.MIME)
	w.Header().Set("Content-Length", strconv.Itoa(len(img.Data)))
	_, _ = w.Write(img.Data)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1<<20)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "multipart field \"file\" is required (maximum 20 MiB)")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxUpload+1))
	if err != nil || len(data) > maxUpload {
		writeErr(w, http.StatusRequestEntityTooLarge, "file exceeds 20 MiB")
		return
	}
	mime := http.DetectContentType(data)
	if !artifact.Supported(mime) {
		writeErr(w, http.StatusUnsupportedMediaType, "only PNG, JPEG, GIF and WebP images are supported")
		return
	}
	id, err := s.artifacts.Save(mime, data)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "name": header.Filename, "mime": mime, "size": len(data), "url": "/artifacts/" + id,
	})
}

func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	path, mime, err := s.artifacts.Path(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "artifact not found")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "artifact not found")
		return
	}
	defer f.Close()
	info, _ := f.Stat()
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// turnOptions resolves the turn's runner and validates its settings against
// what that runner supports. It returns a problem and HTTP status on failure.
func (s *Server) turnOptions(req task.CreateRequest) (task.TurnOptions, string, int) {
	opts := task.TurnOptions{Runner: req.Runner, Model: req.Model, Effort: req.Effort, Attachments: req.Attachments}
	if req.SessionID != "" {
		pinned, err := s.store.SessionRunner(req.SessionID)
		if err != nil {
			return opts, "session not found", http.StatusNotFound
		}
		if opts.Runner != "" && opts.Runner != pinned {
			return opts, task.ErrRunnerMismatch.Error(), http.StatusConflict
		}
		opts.Runner = pinned
	}
	if opts.Runner == "" {
		opts.Runner = s.runners.Default()
	}
	rn, ok := s.runners.Get(opts.Runner)
	if !ok {
		return opts, "runner " + strconv.Quote(opts.Runner) + " is not enabled on this mobd", http.StatusBadRequest
	}
	info := rn.Info()
	if opts.Model != "" && !validModel.MatchString(opts.Model) {
		return opts, "invalid model", http.StatusBadRequest
	}
	if opts.Effort != "" && !slices.Contains(info.Efforts, opts.Effort) {
		if len(info.Efforts) == 0 {
			return opts, info.Label + " has no reasoning effort setting", http.StatusBadRequest
		}
		return opts, "effort must be one of " + strings.Join(info.Efforts, ", "), http.StatusBadRequest
	}
	if len(opts.Attachments) > 0 && !info.Images {
		return opts, info.Label + " does not accept image attachments", http.StatusBadRequest
	}
	if len(opts.Attachments) > 10 {
		return opts, "at most 10 attachments", http.StatusBadRequest
	}
	for _, id := range opts.Attachments {
		if _, _, err := s.artifacts.Path(id); err != nil {
			return opts, "unknown attachment " + strconv.Quote(id), http.StatusBadRequest
		}
	}
	return opts, "", 0
}

// runOptions resolves attachments to files and lets the runner save screenshots.
func (s *Server) runOptions(t *task.Task, env desktop.TurnEnv) runner.Options {
	s.mu.Lock()
	token := s.tokens[t.ID]
	s.mu.Unlock()
	opts := runner.Options{
		SaveImage:    s.artifacts.Save,
		Instructions: askInstructions(),
		// The agent's MCP call must outlive the user's answer time.
		ToolTimeout: s.questionTimeout() + time.Minute,
		MCPEnv:      map[string]string{"MOB_API": s.selfURL(), "MOB_TASK_ID": t.ID, "MOB_TASK_TOKEN": token},
		Ask: func(ctx context.Context, kind, text, details string) (task.Question, error) {
			return s.ask(ctx, t.ID, askRequest{Kind: kind, Text: text, Details: details})
		},
		Env: env.Env, Cgroup: env.Cgroup, OOMScoreAdj: env.OOMScoreAdj,
		Started: func(pid int) {
			s.mu.Lock()
			r := s.reservations[t.ID]
			s.mu.Unlock()
			if r != nil {
				r.Running(t.ID, pid)
			}
		},
	}
	// mobd mcp drives the desktop through mobd serve: the conversation's own desktop on Linux,
	// and on macOS the process holding the Screen Recording grant (a mobd mcp started by an
	// agent CLI is not, and ScreenCaptureKit hangs there instead of refusing).
	opts.MCPEnv["MOB_DESKTOP_PROXY"] = "1"
	for _, id := range t.Attachments {
		if path, _, err := s.artifacts.Path(id); err == nil {
			opts.Images = append(opts.Images, path)
		}
	}
	return opts
}
