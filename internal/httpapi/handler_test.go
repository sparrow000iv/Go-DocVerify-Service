package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sparrow000iv/go-docverify-service/internal/store"
)

// newTestServer builds an isolated server with logging silenced.
func newTestServer() http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(store.New(), logger).Routes()
}

// do issues a request against the handler and returns the recorder.
func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// decode unmarshals a JSON response body into T.
func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %T: %v (body=%s)", v, err, w.Body.String())
	}
	return v
}

// --- health / readiness ----------------------------------------------------

func TestHealthAndReady(t *testing.T) {
	t.Parallel()
	h := newTestServer()

	for _, path := range []string{"/healthz", "/readyz"} {
		w := do(t, h, http.MethodGet, path, "")
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, w.Code)
		}
	}
}

func TestMetricsEndpointExposesCustomCollectors(t *testing.T) {
	t.Parallel()
	h := newTestServer()

	// Generate traffic so the counters are non-zero.
	do(t, h, http.MethodGet, "/healthz", "")

	w := do(t, h, http.MethodGet, "/metrics", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "docverify_http_requests_total") {
		t.Error("metrics output missing docverify_http_requests_total")
	}
}

// --- submit ----------------------------------------------------------------

func TestSubmit_TableDriven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{"valid passport", `{"owner":"tushar","doc_type":"passport","content":"x"}`, http.StatusCreated},
		{"valid national id", `{"owner":"asha","doc_type":"national_id","content":"y"}`, http.StatusCreated},
		{"missing owner", `{"owner":"","doc_type":"passport","content":"x"}`, http.StatusBadRequest},
		{"missing content", `{"owner":"t","doc_type":"passport","content":""}`, http.StatusBadRequest},
		{"unsupported type", `{"owner":"t","doc_type":"library_card","content":"x"}`, http.StatusBadRequest},
		{"malformed json", `{"owner":`, http.StatusBadRequest},
		{"unknown field", `{"owner":"t","doc_type":"passport","content":"x","evil":1}`, http.StatusBadRequest},
		{"empty body", ``, http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newTestServer()
			var w *httptest.ResponseRecorder
			if tc.body == "" {
				r := httptest.NewRequest(http.MethodPost, "/api/v1/documents", bytes.NewReader(nil))
				w = httptest.NewRecorder()
				h.ServeHTTP(w, r)
			} else {
				w = do(t, h, http.MethodPost, "/api/v1/documents", tc.body)
			}
			if w.Code != tc.wantCode {
				t.Errorf("status = %d, want %d (body=%s)", w.Code, tc.wantCode, w.Body.String())
			}
		})
	}
}

// --- full lifecycle --------------------------------------------------------

func TestDocumentLifecycle(t *testing.T) {
	t.Parallel()
	h := newTestServer()

	// Create
	w := do(t, h, http.MethodPost, "/api/v1/documents",
		`{"owner":"tushar","doc_type":"passport","content":"payload"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201", w.Code)
	}
	created := decode[store.Document](t, w)
	if created.Status != store.StatusPending {
		t.Fatalf("status = %q, want PENDING", created.Status)
	}

	// Read
	w = do(t, h, http.MethodGet, "/api/v1/documents/"+created.ID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d, want 200", w.Code)
	}

	// Verify
	w = do(t, h, http.MethodPost, "/api/v1/documents/"+created.ID+"/verify", "")
	if w.Code != http.StatusOK {
		t.Fatalf("verify = %d, want 200", w.Code)
	}
	verified := decode[store.Document](t, w)
	if verified.Status != store.StatusVerified && verified.Status != store.StatusRejected {
		t.Fatalf("status = %q, want terminal", verified.Status)
	}

	// Delete
	w = do(t, h, http.MethodDelete, "/api/v1/documents/"+created.ID, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", w.Code)
	}

	// Confirm gone
	w = do(t, h, http.MethodGet, "/api/v1/documents/"+created.ID, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", w.Code)
	}
}

// --- negative paths --------------------------------------------------------

func TestNotFoundPaths(t *testing.T) {
	t.Parallel()
	h := newTestServer()

	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/documents/missing"},
		{http.MethodPost, "/api/v1/documents/missing/verify"},
		{http.MethodDelete, "/api/v1/documents/missing"},
	}
	for _, c := range cases {
		w := do(t, h, c.method, c.path, "")
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", c.method, c.path, w.Code)
		}
	}
}

func TestList_QueryValidation(t *testing.T) {
	t.Parallel()
	h := newTestServer()

	for i := 0; i < 5; i++ {
		do(t, h, http.MethodPost, "/api/v1/documents",
			`{"owner":"t","doc_type":"passport","content":"c`+string(rune('a'+i))+`"}`)
	}

	w := do(t, h, http.MethodGet, "/api/v1/documents", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200", w.Code)
	}
	got := decode[listResponse](t, w)
	if got.Total != 5 {
		t.Errorf("total = %d, want 5", got.Total)
	}

	if w := do(t, h, http.MethodGet, "/api/v1/documents?limit=2", ""); w.Code == http.StatusOK {
		if g := decode[listResponse](t, w); len(g.Documents) != 2 {
			t.Errorf("limit=2 returned %d docs", len(g.Documents))
		}
	}
	if w := do(t, h, http.MethodGet, "/api/v1/documents?status=BOGUS", ""); w.Code != http.StatusBadRequest {
		t.Errorf("bad status filter = %d, want 400", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/api/v1/documents?limit=abc", ""); w.Code != http.StatusBadRequest {
		t.Errorf("bad limit = %d, want 400", w.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	t.Parallel()
	h := newTestServer()
	w := do(t, h, http.MethodPatch, "/api/v1/documents", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PATCH = %d, want 405", w.Code)
	}
}

func TestResponsesAreJSON(t *testing.T) {
	t.Parallel()
	h := newTestServer()
	w := do(t, h, http.MethodGet, "/api/v1/documents/missing", "")
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}
