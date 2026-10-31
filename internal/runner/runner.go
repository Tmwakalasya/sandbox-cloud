// Package runner executes trusted commands in a single disposable workspace.
// It is designed to run inside a remote Pod, not as a host isolation boundary.
package runner

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
)

const MaxOutput = 1 << 20
const MaxUpload = 8 << 20
const MaxCommands = 32

var commandID = regexp.MustCompile(`^cmd-[a-f0-9]{24}$`)

type Request struct {
	ID             string   `json:"id"`
	Command        []string `json:"command"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

type Result struct {
	ID         string     `json:"id"`
	State      string     `json:"state"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
	Truncated  bool       `json:"truncated"`
}

type Event struct {
	Stream string  `json:"stream"`
	Data   string  `json:"data,omitempty"`
	Result *Result `json:"result,omitempty"`
}

type execution struct {
	request Request
	result  Result
	events  []Event
	bytes   int
	cancel  context.CancelFunc
	done    chan struct{}
	changed chan struct{}
}

// mu serializes command admission and protects results written by stdout/stderr
// reader goroutines. One command may mutate the workspace at a time.
type Server struct {
	mu       sync.Mutex
	root     *os.Root
	workdir  string
	commands map[string]*execution
	order    []string
	active   *execution
	closed   bool
}

func New(workdir string) (*Server, error) {
	path, err := filepath.Abs(workdir)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	return &Server{root: root, workdir: path, commands: make(map[string]*execution)}, nil
}

func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	active := s.active
	if active != nil {
		active.cancel()
	}
	s.mu.Unlock()
	if active != nil {
		<-active.done
	}
	return s.root.Close()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /commands", s.start)
	mux.HandleFunc("GET /commands/{id}", s.inspect)
	mux.HandleFunc("GET /commands/{id}/events", s.events)
	mux.HandleFunc("POST /commands/{id}/cancel", s.cancel)
	mux.HandleFunc("PUT /files", s.put)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func failure(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	var req Request
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		failure(w, 400, "invalid command request: "+err.Error())
		return
	}
	if err := d.Decode(new(any)); err != io.EOF {
		failure(w, 400, "expected one JSON object")
		return
	}
	if req.ID == "" {
		req.ID = "cmd-" + randomID()
	}
	if !commandID.MatchString(req.ID) {
		failure(w, 400, "invalid command ID")
		return
	}
	if req.TimeoutSeconds == 0 {
		req.TimeoutSeconds = 60
	}
	if req.TimeoutSeconds < 1 || req.TimeoutSeconds > 3600 || len(req.Command) == 0 || len(req.Command) > 128 || req.Command[0] == "" {
		failure(w, 400, "provide a command with 1–128 arguments and a timeout between 1 and 3600 seconds")
		return
	}
	for _, arg := range req.Command {
		if strings.ContainsRune(arg, 0) {
			failure(w, 400, "command contains NUL")
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		failure(w, 503, "runner is shutting down")
		return
	}
	if old, ok := s.commands[req.ID]; ok {
		if !reflect.DeepEqual(old.request, req) {
			failure(w, 409, "command ID already used for another request")
			return
		}
		writeJSON(w, 200, old.result)
		return
	}
	if s.active != nil {
		failure(w, 409, "workspace busy")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(req.TimeoutSeconds)*time.Second)
	x := &execution{request: req, result: Result{ID: req.ID, State: "running", StartedAt: time.Now().UTC()}, cancel: cancel, done: make(chan struct{}), changed: make(chan struct{})}
	cmd := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...)
	cmd.Dir = s.workdir
	configureProcess(cmd)
	cmd.WaitDelay = time.Second
	cmd.Stdout = outputWriter{s, x, "stdout"}
	cmd.Stderr = outputWriter{s, x, "stderr"}
	if err := cmd.Start(); err != nil {
		cancel()
		failure(w, 422, "start command: "+err.Error())
		return
	}
	s.active = x
	s.commands[req.ID] = x
	s.order = append(s.order, req.ID)
	if len(s.order) > MaxCommands {
		delete(s.commands, s.order[0])
		s.order = s.order[1:]
	}
	go s.wait(ctx, cmd, x)
	writeJSON(w, 202, x.result)
}

func randomID() string {
	var b [12]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read always fills the buffer or terminates.
	return fmt.Sprintf("%x", b)
}

func (s *Server) wait(ctx context.Context, cmd *exec.Cmd, x *execution) {
	err := cmd.Wait()
	cleanupErr := killGroup(cmd)
	s.mu.Lock()
	defer s.mu.Unlock()
	defer close(x.done)
	defer x.cancel()
	now := time.Now().UTC()
	code := cmd.ProcessState.ExitCode()
	x.result.ExitCode = &code
	x.result.FinishedAt = &now
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		x.result.State = "timed_out"
	case ctx.Err() != nil:
		x.result.State = "cancelled"
	case err != nil:
		x.result.State = "failed"
		x.result.Error = err.Error()
	default:
		x.result.State = "succeeded"
	}
	if cleanupErr != nil {
		x.result.State = "failed"
		x.result.Error = "clean up process group: " + cleanupErr.Error()
	}
	s.active = nil
	close(x.changed)
}

type outputWriter struct {
	s      *Server
	x      *execution
	stream string
}

func (o outputWriter) Write(p []byte) (int, error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	n := len(p)
	remaining := MaxOutput - o.x.bytes
	if len(p) > remaining {
		p = p[:remaining]
		o.x.result.Truncated = true
	}
	// Cap event count as well as byte count for programs emitting tiny writes.
	if len(p) > 0 {
		last := len(o.x.events) - 1
		if last >= 0 && o.x.events[last].Stream == o.stream && len(o.x.events[last].Data)+len(p) <= 8192 {
			o.x.events[last].Data += string(p)
		} else if len(o.x.events) < 4096 {
			o.x.events = append(o.x.events, Event{Stream: o.stream, Data: string(p)})
		} else {
			o.x.result.Truncated = true
			return n, nil
		}
		o.x.bytes += len(p)
		close(o.x.changed)
		o.x.changed = make(chan struct{})
	}
	return n, nil
}

func (s *Server) inspect(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x := s.commands[r.PathValue("id")]
	if x == nil {
		failure(w, 404, "command not found; results are retained for the last 32 commands in this runner lifetime")
		return
	}
	writeJSON(w, 200, x.result)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x := s.commands[r.PathValue("id")]
	if x == nil {
		failure(w, 404, "command not found")
		return
	}
	if x.result.State == "running" {
		x.cancel()
	}
	writeJSON(w, 202, x.result)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	x := s.commands[r.PathValue("id")]
	s.mu.Unlock()
	if x == nil {
		failure(w, 404, "command not found")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return
	}
	enc := json.NewEncoder(w)
	index, offset := 0, 0
	for {
		s.mu.Lock()
		var events []Event
		// The final chunk may grow between notifications; track its byte offset.
		for index < len(x.events) {
			e := x.events[index]
			if offset < len(e.Data) {
				events = append(events, Event{Stream: e.Stream, Data: e.Data[offset:]})
				offset = len(e.Data)
			}
			if index+1 == len(x.events) {
				break
			}
			index++
			offset = 0
		}
		result := x.result
		// Capture the notification under the same lock as the snapshot so an
		// output write between snapshot and select cannot be missed.
		changed := x.changed
		s.mu.Unlock()
		for _, e := range events {
			if err := enc.Encode(e); err != nil {
				return
			}
		}
		if result.State != "running" {
			_ = enc.Encode(Event{Stream: "result", Result: &result})
			_ = rc.Flush()
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-changed:
		}
	}
}

func (s *Server) put(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("path")
	if name == "" || name == "." || !filepath.IsLocal(name) {
		failure(w, 400, "path must be relative to /workspace")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		failure(w, 503, "runner is shutting down")
		return
	}
	if s.active != nil {
		failure(w, 409, "workspace busy")
		return
	}
	if err := s.root.MkdirAll(filepath.Dir(name), 0755); err != nil {
		failure(w, 400, err.Error())
		return
	}
	tmp := filepath.Join(filepath.Dir(name), ".upload-"+randomID())
	f, err := s.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		failure(w, 400, err.Error())
		return
	}
	defer s.root.Remove(tmp)
	n, copyErr := io.Copy(f, http.MaxBytesReader(w, r.Body, MaxUpload))
	closeErr := f.Close()
	if copyErr != nil {
		failure(w, 413, "upload failed: "+copyErr.Error())
		return
	}
	if closeErr != nil {
		failure(w, 500, closeErr.Error())
		return
	}
	if err := s.root.Rename(tmp, name); err != nil {
		failure(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"path": name, "bytes": n})
}
