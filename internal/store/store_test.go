package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fixedClock returns a deterministic clock for reproducible timestamps.
func fixedClock() Clock {
	t := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)
	return func() time.Time { return t }
}

// --- Table-driven validation tests -----------------------------------------

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		owner   string
		docType string
		content string
		wantErr error
	}{
		{"valid passport", "tushar", "passport", "payload", nil},
		{"valid uppercase type", "tushar", "PASSPORT", "payload", nil},
		{"valid national id", "asha", "national_id", "payload", nil},
		{"empty owner", "", "passport", "payload", ErrInvalidOwner},
		{"whitespace owner", "   ", "passport", "payload", ErrInvalidOwner},
		{"empty doc type", "tushar", "", "payload", ErrInvalidType},
		{"empty content", "tushar", "passport", "", ErrInvalidConten},
		{"whitespace content", "tushar", "passport", "  ", ErrInvalidConten},
		{"unsupported type", "tushar", "library_card", "payload", ErrUnsupported},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tc.owner, tc.docType, tc.content)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// --- Submit ----------------------------------------------------------------

func TestSubmit_Success(t *testing.T) {
	t.Parallel()
	s := NewWithClock(fixedClock())

	got, err := s.Submit("  tushar  ", "PASSPORT", "payload")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if got.ID != "doc-000001" {
		t.Errorf("ID = %q, want doc-000001", got.ID)
	}
	if got.Owner != "tushar" {
		t.Errorf("Owner = %q, want trimmed 'tushar'", got.Owner)
	}
	if got.DocType != "passport" {
		t.Errorf("DocType = %q, want lowercased 'passport'", got.DocType)
	}
	if got.Status != StatusPending {
		t.Errorf("Status = %q, want %q", got.Status, StatusPending)
	}
	if got.Score != 0 {
		t.Errorf("Score = %v, want 0 before verification", got.Score)
	}
}

func TestSubmit_IDsAreSequential(t *testing.T) {
	t.Parallel()
	s := NewWithClock(fixedClock())

	for i := 1; i <= 3; i++ {
		d, err := s.Submit("owner", "passport", "payload")
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
		want := fmt.Sprintf("doc-%06d", i)
		if d.ID != want {
			t.Errorf("ID = %q, want %q", d.ID, want)
		}
	}
	if s.Len() != 3 {
		t.Errorf("Len() = %d, want 3", s.Len())
	}
}

func TestSubmit_RejectsInvalid(t *testing.T) {
	t.Parallel()
	s := NewWithClock(fixedClock())

	if _, err := s.Submit("", "passport", "payload"); !errors.Is(err, ErrInvalidOwner) {
		t.Fatalf("error = %v, want ErrInvalidOwner", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len() = %d, want 0 after rejected submit", s.Len())
	}
}

// --- Get / Delete ----------------------------------------------------------

func TestGet_NotFound(t *testing.T) {
	t.Parallel()
	s := NewWithClock(fixedClock())

	if _, err := s.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	s := NewWithClock(fixedClock())

	d, _ := s.Submit("owner", "passport", "payload")
	if err := s.Delete(d.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := s.Delete(d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete() error = %v, want ErrNotFound", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len() = %d, want 0", s.Len())
	}
}

// --- Scoring ---------------------------------------------------------------

func TestScore_IsDeterministic(t *testing.T) {
	t.Parallel()
	a, b := Score("same-content"), Score("same-content")
	if a != b {
		t.Fatalf("Score() not deterministic: %v vs %v", a, b)
	}
}

func TestScore_InRange(t *testing.T) {
	t.Parallel()
	for i := 0; i < 500; i++ {
		v := Score(fmt.Sprintf("content-%d", i))
		if v < 0 || v > 1 {
			t.Fatalf("Score(%d) = %v, out of [0,1]", i, v)
		}
	}
}

// --- Verify ----------------------------------------------------------------

func TestVerify_TransitionsStatus(t *testing.T) {
	t.Parallel()
	s := NewWithClock(fixedClock())

	// Search for inputs that land on each side of the threshold so the test
	// asserts both branches without hard-coding hash values.
	var verifiedID, rejectedID string
	for i := 0; i < 2000 && (verifiedID == "" || rejectedID == ""); i++ {
		content := fmt.Sprintf("sample-%d", i)
		d, err := s.Submit("owner", "passport", content)
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
		if Score(content) >= VerifyThreshold && verifiedID == "" {
			verifiedID = d.ID
		}
		if Score(content) < VerifyThreshold && rejectedID == "" {
			rejectedID = d.ID
		}
	}
	if verifiedID == "" || rejectedID == "" {
		t.Fatal("could not generate both verified and rejected samples")
	}

	v, err := s.Verify(verifiedID)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if v.Status != StatusVerified {
		t.Errorf("Status = %q, want %q (score %v)", v.Status, StatusVerified, v.Score)
	}

	r, err := s.Verify(rejectedID)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if r.Status != StatusRejected {
		t.Errorf("Status = %q, want %q (score %v)", r.Status, StatusRejected, r.Score)
	}
}

func TestVerify_IsIdempotent(t *testing.T) {
	t.Parallel()
	s := NewWithClock(fixedClock())

	d, _ := s.Submit("owner", "passport", "payload")
	first, err := s.Verify(d.ID)
	if err != nil {
		t.Fatalf("first Verify() error = %v", err)
	}
	second, err := s.Verify(d.ID)
	if err != nil {
		t.Fatalf("second Verify() error = %v", err)
	}
	if first.Status != second.Status || first.Score != second.Score {
		t.Errorf("Verify not idempotent: %+v vs %+v", first, second)
	}
}

func TestVerify_NotFound(t *testing.T) {
	t.Parallel()
	s := NewWithClock(fixedClock())
	if _, err := s.Verify("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Verify() error = %v, want ErrNotFound", err)
	}
}

// --- List ------------------------------------------------------------------

func TestList_FilterAndLimit(t *testing.T) {
	t.Parallel()
	s := NewWithClock(fixedClock())

	for i := 0; i < 10; i++ {
		if _, err := s.Submit("owner", "passport", fmt.Sprintf("c-%d", i)); err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
	}

	all, total := s.List("", 0)
	if len(all) != 10 || total != 10 {
		t.Fatalf("List() = %d docs (total %d), want 10/10", len(all), total)
	}

	limited, total := s.List("", 3)
	if len(limited) != 3 {
		t.Errorf("limited len = %d, want 3", len(limited))
	}
	if total != 10 {
		t.Errorf("total = %d, want 10 (limit must not change total)", total)
	}

	pending, _ := s.List(StatusPending, 0)
	if len(pending) != 10 {
		t.Errorf("pending = %d, want 10", len(pending))
	}
	if verified, _ := s.List(StatusVerified, 0); len(verified) != 0 {
		t.Errorf("verified = %d, want 0", len(verified))
	}
}

func TestList_SortedByID(t *testing.T) {
	t.Parallel()
	s := NewWithClock(fixedClock())
	for i := 0; i < 25; i++ {
		s.Submit("owner", "passport", fmt.Sprintf("c-%d", i))
	}
	got, _ := s.List("", 0)
	for i := 1; i < len(got); i++ {
		if got[i-1].ID >= got[i].ID {
			t.Fatalf("not sorted at %d: %q >= %q", i, got[i-1].ID, got[i].ID)
		}
	}
}

// --- Concurrency (run with -race) ------------------------------------------

func TestStore_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	s := NewWithClock(fixedClock())

	const workers = 50
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func(n int) {
			defer wg.Done()
			d, err := s.Submit("owner", "passport", fmt.Sprintf("payload-%d", n))
			if err != nil {
				t.Errorf("Submit() error = %v", err)
				return
			}
			if _, err := s.Verify(d.ID); err != nil {
				t.Errorf("Verify() error = %v", err)
			}
			if _, err := s.Get(d.ID); err != nil {
				t.Errorf("Get() error = %v", err)
			}
			s.List("", 5)
		}(i)
	}
	wg.Wait()

	if s.Len() != workers {
		t.Errorf("Len() = %d, want %d", s.Len(), workers)
	}
}

// --- Benchmarks ------------------------------------------------------------

func BenchmarkSubmit(b *testing.B) {
	s := NewWithClock(fixedClock())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Submit("owner", "passport", "payload")
	}
}

func BenchmarkScore(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Score("some-document-content-to-score")
	}
}

func BenchmarkListParallel(b *testing.B) {
	s := NewWithClock(fixedClock())
	for i := 0; i < 1000; i++ {
		s.Submit("owner", "passport", fmt.Sprintf("c-%d", i))
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.List(StatusPending, 50)
		}
	})
}
