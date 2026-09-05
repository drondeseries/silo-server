package catalog

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
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

func TestIsUnreleasedYearOrDate(t *testing.T) {
	currentYear := time.Now().UTC().Year()

	tests := []struct {
		name        string
		year        int
		releaseDate string
		want        bool
	}{
		{
			name:        "future year is unreleased",
			year:        currentYear + 2,
			releaseDate: "",
			want:        true,
		},
		{
			name:        "past year with no date is released",
			year:        currentYear - 2,
			releaseDate: "",
			want:        false,
		},
		{
			name:        "current year with past festival date is unreleased",
			year:        currentYear,
			releaseDate: fmt.Sprintf("%d-09-05", currentYear-1), // TIFF previous year
			want:        true,
		},
		{
			name:        "future year with past festival date is unreleased",
			year:        currentYear + 1,
			releaseDate: fmt.Sprintf("%d-09-05", currentYear-1),
			want:        true,
		},
		{
			name:        "past year with matching past date is released",
			year:        currentYear - 1,
			releaseDate: fmt.Sprintf("%d-05-15", currentYear-1),
			want:        false,
		},
		{
			name:        "future release date is unreleased",
			year:        currentYear,
			releaseDate: fmt.Sprintf("%d-12-31", currentYear+1),
			want:        true,
		},
		{
			name:        "future release date with timestamp is unreleased",
			year:        currentYear,
			releaseDate: fmt.Sprintf("%d-12-31T00:00:00Z", currentYear+1),
			want:        true,
		},
		{
			name:        "past release date earlier this year is released",
			year:        currentYear,
			releaseDate: fmt.Sprintf("%d-01-01", currentYear),
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isUnreleasedYearOrDate(tt.year, tt.releaseDate)
			if got != tt.want {
				t.Fatalf("isUnreleasedYearOrDate(%d, %q) = %v, want %v", tt.year, tt.releaseDate, got, tt.want)
			}
		})
	}
}

func TestTMDBEntryIsUnreleased(t *testing.T) {
	currentYear := time.Now().UTC().Year()

	tests := []struct {
		name  string
		entry TMDBCollectionEntry
		want  bool
	}{
		{
			name:  "empty release date is unreleased",
			entry: TMDBCollectionEntry{Title: "Placeholder", ReleaseDate: ""},
			want:  true,
		},
		{
			name:  "future release date is unreleased",
			entry: TMDBCollectionEntry{Title: "Upcoming Film", ReleaseDate: fmt.Sprintf("%d-11-20", currentYear+1)},
			want:  true,
		},
		{
			name:  "future release date with timestamp is unreleased",
			entry: TMDBCollectionEntry{Title: "Upcoming Film", ReleaseDate: fmt.Sprintf("%d-11-20T00:00:00Z", currentYear+1)},
			want:  true,
		},
		{
			name:  "past release date for regular movie is released",
			entry: TMDBCollectionEntry{Title: "Released Film", ReleaseDate: fmt.Sprintf("%d-01-15", currentYear-1)},
			want:  false,
		},
		{
			name: "past festival date with current year in title is unreleased",
			entry: TMDBCollectionEntry{
				Title:       fmt.Sprintf("Fuze (%d)", currentYear),
				ReleaseDate: fmt.Sprintf("%d-09-05", currentYear-1), // TIFF premiere in prior year
			},
			want: true,
		},
		{
			name: "past festival date with future year in title is unreleased",
			entry: TMDBCollectionEntry{
				Title:       fmt.Sprintf("Future Film (%d)", currentYear+1),
				ReleaseDate: fmt.Sprintf("%d-09-05", currentYear-1),
			},
			want: true,
		},
		{
			name:  "unparseable date is unreleased",
			entry: TMDBCollectionEntry{Title: "Corrupt Date", ReleaseDate: "not-a-date"},
			want:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tmdbEntryIsUnreleased(tt.entry)
			if got != tt.want {
				t.Fatalf("tmdbEntryIsUnreleased(%+v) = %v, want %v", tt.entry, got, tt.want)
			}
		})
	}
}
