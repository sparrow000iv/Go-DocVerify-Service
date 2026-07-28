// Package httpapi implements the REST transport for the DocVerify service
// using only the Go standard library's net/http router (Go 1.22+ patterns).
package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/sparrow000iv/go-docverify-service/internal/metrics"
	"github.com/sparrow000iv/go-docverify-service/internal/store"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server wires the document store to HTTP handlers.
type Server struct {
	store  *store.Store
	logger *slog.Logger
}

// New returns an HTTP server backed by the given store.
func New(s *store.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{store: s, logger: logger}
}

// Routes builds the http.Handler with all routes and middleware applied.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.Handle("GET /metrics", promhttp.Handler())

	mux.HandleFunc("POST /api/v1/documents", s.handleSubmit)
	mux.HandleFunc("GET /api/v1/documents", s.handleList)
	mux.HandleFunc("GET /api/v1/documents/{id}", s.handleGet)
	mux.HandleFunc("POST /api/v1/documents/{id}/verify", s.handleVerify)
	mux.HandleFunc("DELETE /api/v1/documents/{id}", s.handleDelete)

	return s.withObservability(mux)
}

// statusRecorder captures the status code for metrics and access logs.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// withObservability records metrics and emits a structured access log line.
func (s *Server) withObservability(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		metrics.ObserveHTTP(r.Method, route, rec.status, started)
		s.logger.Info("http_request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	})
}

// --- response helpers ------------------------------------------------------

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, errorBody{Error: msg})
}

// statusFor maps domain errors onto HTTP status codes.
func statusFor(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, store.ErrInvalidOwner),
		errors.Is(err, store.ErrInvalidType),
		errors.Is(err, store.ErrInvalidConten),
		errors.Is(err, store.ErrUnsupported):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// --- handlers --------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ready",
		"docs":   s.store.Len(),
	})
}

type submitRequest struct {
	Owner   string `json:"owner"`
	DocType string `json:"doc_type"`
	Content string `json:"content"`
}

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var req submitRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	doc, err := s.store.Submit(req.Owner, req.DocType, req.Content)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	metrics.DocumentsStored.Set(float64(s.store.Len()))
	writeJSON(w, http.StatusCreated, doc)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	doc, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

type listResponse struct {
	Documents []store.Document `json:"documents"`
	Total     int              `json:"total"`
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var filter store.Status
	if v := q.Get("status"); v != "" {
		switch store.Status(v) {
		case store.StatusPending, store.StatusVerified, store.StatusRejected:
			filter = store.Status(v)
		default:
			writeErr(w, http.StatusBadRequest, "invalid status filter: "+v)
			return
		}
	}

	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "invalid limit: "+v)
			return
		}
		limit = n
	}

	docs, total := s.store.List(filter, limit)
	writeJSON(w, http.StatusOK, listResponse{Documents: docs, Total: total})
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	doc, err := s.store.Verify(r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	metrics.DocumentsTotal.WithLabelValues(string(doc.Status)).Inc()
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Delete(r.PathValue("id")); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	metrics.DocumentsStored.Set(float64(s.store.Len()))
	w.WriteHeader(http.StatusNoContent)
}
