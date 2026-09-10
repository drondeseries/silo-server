package scanner

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestClearVirtualCandidateFailed verifies the failed_at stamp lifecycle for
// the batched version liveness check: ClearVirtualCandidateFailed clears the
// stamp on a virtual row, is a no-op for local rows, and is a no-op for
// vanished rows.
func TestClearVirtualCandidateFailed(t *testing.T) {
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
	contentID := fmt.Sprintf("clear-virtual-failed-%d", suffix)

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Clear Virtual Failed %d", suffix)).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Clear Virtual Failed','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}

	repo := NewFileRepository(pool)

	// Virtual candidate row (container='virtual', virtual:// path).
	var virtualID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id,failed_at)
		VALUES($1,$2,$3,1000,'virtual',7,NOW()) RETURNING id`,
		contentID, folderID, fmt.Sprintf("virtual://movie/tt%d?result=dead", suffix)).Scan(&virtualID); err != nil {
		t.Fatalf("seed virtual file: %v", err)
	}

	// Local row with a failed_at stamp (should never be touched by the clear).
	var localID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,failed_at)
		VALUES($1,$2,$3,1000,NOW()) RETURNING id`,
		contentID, folderID, fmt.Sprintf("/media/clear-virtual-failed-%d.mkv", suffix)).Scan(&localID); err != nil {
		t.Fatalf("seed local file: %v", err)
	}

	assertFailedAt := func(fileID int, want bool) {
		t.Helper()
		var failedAt *time.Time
		if err := pool.QueryRow(ctx, `SELECT failed_at FROM media_files WHERE id=$1`, fileID).Scan(&failedAt); err != nil {
			t.Fatalf("read failed_at for file %d: %v", fileID, err)
		}
		if (failedAt != nil) != want {
			t.Fatalf("file %d failed_at set=%v, want %v", fileID, failedAt != nil, want)
		}
	}

	// Clear on the virtual row removes the stamp.
	if err := repo.ClearVirtualCandidateFailed(ctx, virtualID); err != nil {
		t.Fatalf("clear virtual candidate failed: %v", err)
	}
	assertFailedAt(virtualID, false)

	// Clear on a local row is a no-op: the stamp survives.
	if err := repo.ClearVirtualCandidateFailed(ctx, localID); err != nil {
		t.Fatalf("clear local candidate failed: %v", err)
	}
	assertFailedAt(localID, true)

	// Clear on a vanished row is a no-op (no error).
	if err := repo.ClearVirtualCandidateFailed(ctx, 999999999); err != nil {
		t.Fatalf("clear vanished candidate failed: %v", err)
	}

	// MarkVirtualCandidateFailed still stamps the virtual row (round-trip).
	if err := repo.MarkVirtualCandidateFailed(ctx, virtualID); err != nil {
		t.Fatalf("mark virtual candidate failed: %v", err)
	}
	assertFailedAt(virtualID, true)

	// The model round-trips the stamp through the repository.
	file, err := repo.GetByID(ctx, virtualID)
	if err != nil {
		t.Fatalf("get virtual file: %v", err)
	}
	if file == nil || file.FailedAt == nil {
		t.Fatalf("virtual file failed_at not loaded: %#v", file)
	}
	if file.MissingSince != nil {
		t.Fatalf("virtual file must never carry missing_since: %#v", file)
	}
}
