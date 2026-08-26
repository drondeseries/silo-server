package catalog

import (
	"context"
	"errors"
	"testing"
)

type fakeDigitalReleaseChecker struct {
	released map[int]bool
	err      error
	calls    map[int]int
}

func (f *fakeDigitalReleaseChecker) HasDigitalRelease(_ context.Context, tmdbID int) (bool, error) {
	if f.calls == nil {
		f.calls = map[int]int{}
	}
	f.calls[tmdbID]++
	if f.err != nil {
		return false, f.err
	}
	return f.released[tmdbID], nil
}

func TestTheatricalReleaseGateSkipsTheatricalOnlyMovies(t *testing.T) {
	checker := &fakeDigitalReleaseChecker{released: map[int]bool{
		100: false, // theatrical-only
		200: true,  // digitally released
	}}
	gate := newTheatricalReleaseGate(checker)
	ctx := context.Background()

	if !gate.skipTheatricalMovie(ctx, 100, "Theatrical Movie") {
		t.Fatal("a theatrical-only movie must be skipped")
	}
	if gate.skipTheatricalMovie(ctx, 200, "Digital Movie") {
		t.Fatal("a digitally released movie must not be skipped")
	}
}

func TestTheatricalReleaseGateFailsOpen(t *testing.T) {
	gate := newTheatricalReleaseGate(&fakeDigitalReleaseChecker{err: errors.New("tmdb down")})
	if gate.skipTheatricalMovie(context.Background(), 300, "Outage Movie") {
		t.Fatal("a checker failure must fail open (materialize)")
	}

	var nilChecker *fakeDigitalReleaseChecker
	failingGate := newTheatricalReleaseGate(nil)
	_ = nilChecker
	if failingGate.skipTheatricalMovie(context.Background(), 300, "No Checker") {
		t.Fatal("a nil checker must fail open")
	}
}

func TestTheatricalReleaseGateSkipsLookupWithoutTMDBID(t *testing.T) {
	checker := &fakeDigitalReleaseChecker{released: map[int]bool{}}
	gate := newTheatricalReleaseGate(checker)
	if gate.skipTheatricalMovie(context.Background(), 0, "No TMDB ID") {
		t.Fatal("entries without a TMDB ID must fall through to the date gates")
	}
	if len(checker.calls) != 0 {
		t.Fatalf("lookup calls = %d, want 0 for missing TMDB ID", len(checker.calls))
	}
}

func TestTheatricalReleaseGateMemoizesLookupsPerRun(t *testing.T) {
	checker := &fakeDigitalReleaseChecker{released: map[int]bool{400: false}}
	gate := newTheatricalReleaseGate(checker)
	ctx := context.Background()

	for range 3 {
		if !gate.skipTheatricalMovie(ctx, 400, "Memoized") {
			t.Fatal("theatrical-only movie must be skipped on every call")
		}
	}
	if checker.calls[400] != 1 {
		t.Fatalf("checker calls = %d, want 1 (memoized per sync run)", checker.calls[400])
	}
}
