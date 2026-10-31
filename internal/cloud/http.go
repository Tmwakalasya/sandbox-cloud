package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

var commandID = regexp.MustCompile(`^cmd-[a-f0-9]{24}$`)

type Server struct {
	Kube      *Kubernetes
	Transport http.RoundTripper
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, 200, map[string]string{"status": "ok"}) })
	m.HandleFunc("POST /v1/workspaces", s.create)
	m.HandleFunc("GET /v1/workspaces", s.list)
	m.HandleFunc("GET /v1/workspaces/{id}", s.get)
	m.HandleFunc("DELETE /v1/workspaces/{id}", s.destroy)
	m.HandleFunc("PUT /v1/workspaces/{id}/files", s.forward)
	m.HandleFunc("POST /v1/workspaces/{id}/commands", s.forward)
	m.HandleFunc("GET /v1/workspaces/{id}/commands/{command}", s.forward)
	m.HandleFunc("GET /v1/workspaces/{id}/commands/{command}/events", s.forward)
	m.HandleFunc("POST /v1/workspaces/{id}/commands/{command}/cancel", s.forward)
	return m
}
func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func respondError(w http.ResponseWriter, err error) {
	status := 502
	var own *Error
	switch {
	case errors.As(err, &own):
		status = own.Status
	case apierrors.IsNotFound(err):
		status = 404
	case apierrors.IsAlreadyExists(err), apierrors.IsConflict(err):
		status = 409
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		status = 400
	case apierrors.IsForbidden(err):
		status = 403
	case apierrors.IsTooManyRequests(err):
		status = 429
	case errors.Is(err, context.DeadlineExceeded):
		status = 504
	}
	jsonResponse(w, status, map[string]string{"error": err.Error()})
}
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		respondError(w, &Error{400, "invalid create request: " + err.Error()})
		return
	}
	if err := d.Decode(new(any)); err != io.EOF {
		respondError(w, &Error{400, "expected one JSON object"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	result, err := s.Kube.Create(ctx, req)
	if err != nil {
		respondError(w, err)
		return
	}
	jsonResponse(w, 201, result)
}
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	result, err := s.Kube.List(ctx)
	if err != nil {
		respondError(w, err)
		return
	}
	jsonResponse(w, 200, result)
}
func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	result, err := s.Kube.Get(ctx, r.PathValue("id"))
	if err != nil {
		respondError(w, err)
		return
	}
	jsonResponse(w, 200, result)
}
func (s *Server) destroy(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.Kube.Destroy(ctx, r.PathValue("id")); err != nil {
		respondError(w, err)
		return
	}
	jsonResponse(w, 202, map[string]string{"state": "deletion_requested"})
}
func (s *Server) forward(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cmd := r.PathValue("command")
	if cmd != "" && !commandID.MatchString(cmd) {
		respondError(w, &Error{400, "invalid command ID"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	address, err := s.Kube.RunnerAddress(ctx, id)
	cancel()
	if err != nil {
		respondError(w, err)
		return
	}
	target, err := url.Parse(address)
	if err != nil {
		respondError(w, err)
		return
	}
	path := r.URL.Path[len("/v1/workspaces/"+id):]
	transport := s.Transport
	if transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = nil
		t.ResponseHeaderTimeout = 15 * time.Second
		transport = t
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(target)
			p.Out.URL.Path = path
			p.Out.URL.RawPath = ""
			// No user credentials are forwarded into a workspace.
			p.Out.Header.Del("Authorization")
			p.Out.Header.Del("Cookie")
		},
		Transport: transport, FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			respondError(w, errors.New("runner unavailable; command outcome may be unknown: "+err.Error()))
		},
	}
	proxy.ServeHTTP(w, r)
}

func (s *Server) Reconcile(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		pass, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := s.Kube.Reap(pass)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("expiry reconciliation: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
