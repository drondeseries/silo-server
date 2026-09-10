package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/scanner"
)

// TestCatalogVersionsCheckHTTP exercises the batched version liveness check
// against a real database: dead pins are stamped failed_at and reported
// unavailable, live candidates are cleared and reported available, local
// missing files are reported unavailable without any provider probe, and
// ambiguous provider errors leave the stamp unchanged.
func TestCatalogVersionsCheckHTTP(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	prefix := fmt.Sprintf("versions-check-%d-", time.Now().UnixNano())
	var library int
	if err := pool.QueryRow(t.Context(), `INSERT INTO media_folders (type,name) VALUES ('movies',$1) RETURNING id`, prefix).Scan(&library); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, library); err != nil {
			t.Error(err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM media_items WHERE content_id LIKE $1`, prefix+"%"); err != nil {
			t.Error(err)
		}
	})
	exec(`INSERT INTO media_items (content_id,type,title,genres,default_metadata_language) VALUES ($1,'movie','Versions Check','{}','en')`, prefix+"movie")
	exec(`INSERT INTO media_item_libraries (content_id,media_folder_id) VALUES ($1,$2)`, prefix+"movie", library)

	insertFile := func(path, container string, owner int, failedAt, missingSince any) int {
		t.Helper()
		var id int
		if err := pool.QueryRow(t.Context(), `
			INSERT INTO media_files (content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id,failed_at,missing_since)
			VALUES ($1,$2,$3,1000,$4,$5,$6,$7) RETURNING id`,
			prefix+"movie", library, path, container, owner, failedAt, missingSince).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	deadPinID := insertFile(fmt.Sprintf("virtual://movie/tt%d?result=dead", time.Now().UnixNano()), "virtual", 7, nil, nil)
	livePinID := insertFile(fmt.Sprintf("virtual://movie/tt%d?result=live", time.Now().UnixNano()), "virtual", 7, time.Now(), nil)
	missingLocalID := insertFile(fmt.Sprintf("/media/%s-missing.mkv", prefix), "", 0, nil, time.Now())
	presentLocalID := insertFile(fmt.Sprintf("/media/%s-present.mkv", prefix), "", 0, nil, nil)
	ambiguousID := insertFile(fmt.Sprintf("virtual://movie/tt%d?result=ambiguous", time.Now().UnixNano()), "virtual", 7, nil, nil)
	joinedAmbiguousID := insertFile(fmt.Sprintf("virtual://movie/tt%d?result=joined", time.Now().UnixNano()), "virtual", 7, nil, nil)

	var resolveCalls atomic.Int64
	var deadPinCalls atomic.Int64
	resolver := VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		resolveCalls.Add(1)
		switch {
		case strings.Contains(uri, "result=dead"):
			deadPinCalls.Add(1)
			return ResolvedVirtualMedia{}, errors.New("virtual stream provider returned no matching candidate")
		case strings.Contains(uri, "result=live"):
			return ResolvedVirtualMedia{URL: "http://provider.test/live.mp4", URI: uri, CandidateID: "live"}, nil
		case strings.Contains(uri, "result=ambiguous"):
			return ResolvedVirtualMedia{}, errors.New("virtual stream provider 7 request failed")
		case strings.Contains(uri, "result=joined"):
			// Owner provider RPC-failed while a fallback answered with no
			// matching candidate: the pin's owner may simply be down.
			return ResolvedVirtualMedia{}, errors.New("resolve virtual playback: virtual stream provider 7 request failed; virtual stream provider returned no matching candidate")
		default:
			return ResolvedVirtualMedia{}, errors.New("unexpected uri")
		}
	})

	repo := scanner.NewFileRepository(pool)
	h := &CatalogResourceHandler{
		FileResolver:       repo,
		VirtualResolver:    resolver,
		MarkVirtualFailed:  repo.MarkVirtualCandidateFailed,
		ClearVirtualFailed: repo.ClearVirtualCandidateFailed,
	}
	router := chi.NewRouter()
	router.Post("/catalog/versions/check", h.HandleCheckVersions)

	post := func(ids ...int) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string][]int{"file_ids": ids})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/catalog/versions/check", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		return rec
	}

	readFailedAt := func(fileID int) bool {
		t.Helper()
		var failedAt *time.Time
		if err := pool.QueryRow(t.Context(), `SELECT failed_at FROM media_files WHERE id=$1`, fileID).Scan(&failedAt); err != nil {
			t.Fatal(err)
		}
		return failedAt != nil
	}

	t.Run("dead pin stamped and reported unavailable", func(t *testing.T) {
		rec := post(deadPinID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp versionCheckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Results) != 1 || resp.Results[0].FileID != deadPinID || resp.Results[0].Available {
			t.Fatalf("unexpected results: %#v", resp.Results)
		}
		if !readFailedAt(deadPinID) {
			t.Fatal("dead pin was not stamped failed_at")
		}
		if deadPinCalls.Load() != 1 {
			t.Fatalf("dead pin resolver calls = %d, want 1", deadPinCalls.Load())
		}
	})

	t.Run("live candidate cleared and reported available", func(t *testing.T) {
		if !readFailedAt(livePinID) {
			t.Fatal("precondition: live candidate must start stamped failed")
		}
		rec := post(livePinID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp versionCheckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Results) != 1 || resp.Results[0].FileID != livePinID || !resp.Results[0].Available {
			t.Fatalf("unexpected results: %#v", resp.Results)
		}
		if readFailedAt(livePinID) {
			t.Fatal("live candidate failed_at was not cleared")
		}
	})

	t.Run("local missing file unavailable without probe", func(t *testing.T) {
		before := resolveCalls.Load()
		rec := post(missingLocalID, presentLocalID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp versionCheckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		byID := map[int]bool{}
		for _, r := range resp.Results {
			byID[r.FileID] = r.Available
		}
		if byID[missingLocalID] {
			t.Fatalf("missing local file reported available: %#v", resp.Results)
		}
		if !byID[presentLocalID] {
			t.Fatalf("present local file reported unavailable: %#v", resp.Results)
		}
		if resolveCalls.Load() != before {
			t.Fatalf("local files triggered %d provider probes, want 0", resolveCalls.Load()-before)
		}
	})

	t.Run("ambiguous provider error leaves stamp unchanged", func(t *testing.T) {
		rec := post(ambiguousID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp versionCheckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Results) != 1 || resp.Results[0].FileID != ambiguousID || !resp.Results[0].Available {
			t.Fatalf("unexpected results: %#v", resp.Results)
		}
		if readFailedAt(ambiguousID) {
			t.Fatal("ambiguous provider error must not stamp failed_at")
		}
	})

	t.Run("joined owner-down error leaves stamp unchanged", func(t *testing.T) {
		rec := post(joinedAmbiguousID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp versionCheckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Results) != 1 || resp.Results[0].FileID != joinedAmbiguousID || !resp.Results[0].Available {
			t.Fatalf("unexpected results: %#v", resp.Results)
		}
		if readFailedAt(joinedAmbiguousID) {
			t.Fatal("joined owner-down error must not stamp failed_at")
		}
	})

	t.Run("unknown file id reported unavailable", func(t *testing.T) {
		rec := post(999999999)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var resp versionCheckResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Results) != 1 || resp.Results[0].FileID != 999999999 || resp.Results[0].Available {
			t.Fatalf("unexpected results: %#v", resp.Results)
		}
	})

	t.Run("batch over cap rejected", func(t *testing.T) {
		ids := make([]int, maxVersionCheckFiles+1)
		for i := range ids {
			ids[i] = i + 1
		}
		rec := post(ids...)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status %d, want 413: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("empty batch rejected", func(t *testing.T) {
		rec := post()
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})
}
