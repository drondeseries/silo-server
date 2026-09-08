package scanner

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/scanbatch"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestFileRepositoryUpsertRoundTripReleaseMetadata verifies the Upsert
// round-trips the release metadata and probe-version columns added for the
// MULTI-audio + release name/group work: the languages array on audio tracks,
// release_name/release_group, and probe_version.
func TestFileRepositoryUpsertRoundTripReleaseMetadata(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)

	suffix := time.Now().UnixNano()
	movieID := fmt.Sprintf("release-metadata-movie-%d", suffix)
	runID := fmt.Sprintf("release-metadata-run-%d", suffix)
	var folderID int
	if err := pool.QueryRow(ctx, `INSERT INTO media_folders (type, name, enabled) VALUES ('movies', $1, true) RETURNING id`, fmt.Sprintf("release-metadata-folder-%d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id = $1`, movieID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id = $1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items (content_id, type, title, status, genres)
		VALUES ($1, 'movie', 'Release Metadata', 'matched', '{}'::text[])
	`, movieID); err != nil {
		t.Fatalf("seed movie: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_item_libraries (content_id, media_folder_id) VALUES ($1, $2)`, movieID, folderID); err != nil {
		t.Fatalf("seed movie membership: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO scan_runs (id, media_folder_id, mode, status)
		VALUES ($1, $2, 'library', 'completed')
	`, runID, folderID); err != nil {
		t.Fatalf("seed scan run: %v", err)
	}

	repo := NewFileRepository(pool)
	filePath := fmt.Sprintf("/tmp/release-metadata-%d-movie.mkv", suffix)
	file, err := repo.Upsert(scanbatch.WithRunID(ctx, runID), models.MediaFile{
		ContentID:     movieID,
		MediaFolderID: folderID,
		FilePath:      filePath,
		FileSize:      1024,
		AudioTracks: []models.AudioTrack{
			{Language: "mul", Languages: []string{"en", "fr", "es"}, Codec: "eac3"},
		},
		ReleaseName:  "Movie.2023.2160p.Multi-AltMount",
		ReleaseGroup: "AltMount",
		ProbeVersion: 1,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if len(file.AudioTracks) != 1 {
		t.Fatalf("audio tracks = %d, want 1", len(file.AudioTracks))
	}
	if !reflect.DeepEqual(file.AudioTracks[0].Languages, []string{"en", "fr", "es"}) {
		t.Fatalf("audio Languages = %v, want [en fr es]", file.AudioTracks[0].Languages)
	}
	if file.AudioTracks[0].Language != "mul" {
		t.Fatalf("audio Language = %q, want mul", file.AudioTracks[0].Language)
	}
	if file.ReleaseName != "Movie.2023.2160p.Multi-AltMount" {
		t.Fatalf("ReleaseName = %q, want Movie.2023.2160p.Multi-AltMount", file.ReleaseName)
	}
	if file.ReleaseGroup != "AltMount" {
		t.Fatalf("ReleaseGroup = %q, want AltMount", file.ReleaseGroup)
	}
	if file.ProbeVersion != 1 {
		t.Fatalf("ProbeVersion = %d, want 1", file.ProbeVersion)
	}

	// A re-scan without the new fields keeps them (the DO UPDATE path does not
	// blank columns the caller did not set).
	rescanned, err := repo.Upsert(scanbatch.WithRunID(ctx, runID), models.MediaFile{
		ContentID:     movieID,
		MediaFolderID: folderID,
		FilePath:      filePath,
		FileSize:      2048,
		AudioTracks: []models.AudioTrack{
			{Language: "mul", Languages: []string{"en", "fr", "es"}, Codec: "eac3"},
		},
	})
	if err != nil {
		t.Fatalf("rescan upsert: %v", err)
	}
	if rescanned.ReleaseName != "Movie.2023.2160p.Multi-AltMount" {
		t.Fatalf("rescan ReleaseName = %q, want retained", rescanned.ReleaseName)
	}
	if rescanned.ReleaseGroup != "AltMount" {
		t.Fatalf("rescan ReleaseGroup = %q, want retained", rescanned.ReleaseGroup)
	}
	if rescanned.ProbeVersion != 0 {
		t.Fatalf("rescan ProbeVersion = %d, want 0 (default when unset)", rescanned.ProbeVersion)
	}
}
