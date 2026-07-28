// Package store provides the in-memory document repository and the
// verification scoring logic for the DocVerify service.
//
// The store is safe for concurrent use: every exported method acquires the
// appropriate read or write lock. This is the component exercised most
// heavily by the race-detector tests in store_test.go.
package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Sentinel errors returned by the store. Callers use errors.Is to branch,
// and the transport layers map these onto HTTP codes / gRPC status codes.
var (
	ErrNotFound      = errors.New("document not found")
	ErrInvalidOwner  = errors.New("owner must not be empty")
	ErrInvalidType   = errors.New("doc_type must not be empty")
	ErrInvalidConten = errors.New("content must not be empty")
	ErrUnsupported   = errors.New("unsupported doc_type")
)

// Status models the verification lifecycle of a document.
type Status string

const (
	StatusPending  Status = "PENDING"
	StatusVerified Status = "VERIFIED"
	StatusRejected Status = "REJECTED"
)

// SupportedTypes is the allow-list of document types the service accepts.
var SupportedTypes = map[string]bool{
	"passport":       true,
	"drivers_licens": true,
	"national_id":    true,
	"utility_bill":   true,
}

// VerifyThreshold is the score at or above which a document is marked
// verified. Documents scoring below it are rejected.
const VerifyThreshold = 0.60

// Document is the core domain entity.
type Document struct {
	ID        string    `json:"id"`
	Owner     string    `json:"owner"`
	DocType   string    `json:"doc_type"`
	Status    Status    `json:"status"`
	Score     float64   `json:"score"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// content is unexported: it never leaves the process boundary.
	content string
}

// Clock lets tests inject deterministic time instead of wall-clock time.
type Clock func() time.Time

// Store is a concurrency-safe in-memory document repository.
type Store struct {
	mu    sync.RWMutex
	docs  map[string]*Document
	clock Clock
	seq   atomic.Uint64
}

// New returns a Store using the real wall clock.
func New() *Store { return NewWithClock(time.Now) }

// NewWithClock returns a Store using the supplied clock. Tests pass a fixed
// clock so that CreatedAt / UpdatedAt assertions are deterministic.
func NewWithClock(c Clock) *Store {
	if c == nil {
		c = time.Now
	}
	return &Store{docs: make(map[string]*Document), clock: c}
}

// nextID produces a monotonic, collision-free identifier.
func (s *Store) nextID() string {
	return fmt.Sprintf("doc-%06d", s.seq.Add(1))
}

// Validate checks a submission before it is accepted. It is exported so the
// transport layers can reject bad input before touching the store.
func Validate(owner, docType, content string) error {
	if strings.TrimSpace(owner) == "" {
		return ErrInvalidOwner
	}
	if strings.TrimSpace(docType) == "" {
		return ErrInvalidType
	}
	if strings.TrimSpace(content) == "" {
		return ErrInvalidConten
	}
	if !SupportedTypes[strings.ToLower(docType)] {
		return fmt.Errorf("%w: %q", ErrUnsupported, docType)
	}
	return nil
}

// Submit validates and stores a new document in PENDING state.
func (s *Store) Submit(owner, docType, content string) (Document, error) {
	if err := Validate(owner, docType, content); err != nil {
		return Document{}, err
	}
	now := s.clock().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	d := &Document{
		ID:        s.nextID(),
		Owner:     strings.TrimSpace(owner),
		DocType:   strings.ToLower(strings.TrimSpace(docType)),
		Status:    StatusPending,
		Score:     0,
		CreatedAt: now,
		UpdatedAt: now,
		content:   content,
	}
	s.docs[d.ID] = d
	return *d, nil
}

// Get returns a copy of the document with the given id.
func (s *Store) Get(id string) (Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	d, ok := s.docs[id]
	if !ok {
		return Document{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return *d, nil
}

// List returns documents sorted by ID. An empty statusFilter returns all
// documents; limit <= 0 means no limit.
func (s *Store) List(statusFilter Status, limit int) ([]Document, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Document, 0, len(s.docs))
	for _, d := range s.docs {
		if statusFilter != "" && d.Status != statusFilter {
			continue
		}
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	total := len(out)
	if limit > 0 && limit < total {
		out = out[:limit]
	}
	return out, total
}

// Score computes a deterministic confidence score in [0,1] for the given
// content. Using SHA-256 keeps the result stable across runs and platforms,
// which is what makes the table-driven tests reproducible.
func Score(content string) float64 {
	sum := sha256.Sum256([]byte(content))
	// Fold the first four bytes into a uint32 and normalise.
	v := uint32(sum[0])<<24 | uint32(sum[1])<<16 | uint32(sum[2])<<8 | uint32(sum[3])
	return float64(v) / float64(^uint32(0))
}

// Verify scores a stored document and transitions it to VERIFIED or
// REJECTED. Verifying an already-terminal document is idempotent.
func (s *Store) Verify(id string) (Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.docs[id]
	if !ok {
		return Document{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if d.Status != StatusPending {
		return *d, nil // idempotent: already decided
	}

	d.Score = Score(d.content)
	if d.Score >= VerifyThreshold {
		d.Status = StatusVerified
	} else {
		d.Status = StatusRejected
	}
	d.UpdatedAt = s.clock().UTC()
	return *d, nil
}

// Delete removes a document, reporting whether it existed.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.docs[id]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	delete(s.docs, id)
	return nil
}

// Len reports the number of stored documents.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.docs)
}
