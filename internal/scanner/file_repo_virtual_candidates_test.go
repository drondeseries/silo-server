package scanner

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestReplaceVirtualCandidatesIsScopedToLibrary(t *testing.T) {
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
	contentID := fmt.Sprintf("virtual-candidate-scope-%d", suffix)
	basePath := fmt.Sprintf("virtual://movie/tt%d?profile=1080p", suffix)
	firstPath := basePath + "&result=first"
	secondPath := basePath + "&result=second"
	var folderA, folderB int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Candidate A %d", suffix)).Scan(&folderA); err != nil {
		t.Fatalf("seed first folder: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Candidate B %d", suffix)).Scan(&folderB); err != nil {
		t.Fatalf("seed second folder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_files WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_item_libraries WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_items WHERE content_id=$1`, contentID)
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=ANY($1::int[])`, []int{folderA, folderB})
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_items(content_id,type,title,status,genres)
		VALUES($1,'movie','Candidate Scope','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed candidate item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2),($1,$3)`, contentID, folderA, folderB); err != nil {
		t.Fatalf("seed candidate library links: %v", err)
	}

	repo := NewFileRepository(pool)
	source := func(folderID int) *models.MediaFile {
		return &models.MediaFile{
			ContentID:                  contentID,
			MediaFolderID:              folderID,
			FilePath:                   basePath,
			VirtualOwnerInstallationID: 91,
		}
	}
	first := []VirtualCandidate{{URI: firstPath, Label: "1080p"}}
	if err := repo.ReplaceVirtualCandidates(ctx, source(folderA), first); err != nil {
		t.Fatalf("replace first-library candidates: %v", err)
	}
	if err := repo.ReplaceVirtualCandidates(ctx, source(folderB), first); err != nil {
		t.Fatalf("replace second-library candidates: %v", err)
	}
	if err := repo.ReplaceVirtualCandidates(ctx, source(folderA), []VirtualCandidate{{URI: secondPath, Label: "1080p"}}); err != nil {
		t.Fatalf("refresh first-library candidates: %v", err)
	}

	var firstA, firstB, secondA int
	if err := pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER(WHERE media_folder_id=$2 AND file_path=$4),
		  count(*) FILTER(WHERE media_folder_id=$3 AND file_path=$4),
		  count(*) FILTER(WHERE media_folder_id=$2 AND file_path=$5)
		FROM media_files
		WHERE content_id=$1 AND virtual_owner_installation_id=91`,
		contentID, folderA, folderB, firstPath, secondPath,
	).Scan(&firstA, &firstB, &secondA); err != nil {
		t.Fatalf("inspect library-scoped candidates: %v", err)
	}
	if firstA != 0 || firstB != 1 || secondA != 1 {
		t.Fatalf("candidate rows firstA=%d firstB=%d secondA=%d, want 0/1/1", firstA, firstB, secondA)
	}
}

func TestReplaceVirtualCandidatesPreservesBarePlaceholder(t *testing.T) {
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
	contentID := fmt.Sprintf("virtual-bare-placeholder-%d", suffix)
	barePath := fmt.Sprintf("virtual://movie/tt%d", suffix)
	candidatePath := barePath + "?result=beststream123"

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Placeholder Folder %d", suffix)).Scan(&folderID); err != nil {
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
		VALUES($1,'movie','Placeholder Scope','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	// Seed initial bare placeholder file
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES($1,$2,$3,0,'mkv',5)`, contentID, folderID, barePath); err != nil {
		t.Fatalf("seed bare placeholder file: %v", err)
	}

	repo := NewFileRepository(pool)
	source := &models.MediaFile{
		ContentID:                  contentID,
		MediaFolderID:              folderID,
		FilePath:                   barePath,
		VirtualOwnerInstallationID: 5,
	}

	if err := repo.ReplaceVirtualCandidates(ctx, source, []VirtualCandidate{{URI: candidatePath, Label: "2160p"}}); err != nil {
		t.Fatalf("replace virtual candidates: %v", err)
	}

	var bareCount, candidateCount int
	if err := pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER(WHERE file_path=$2),
		  count(*) FILTER(WHERE file_path=$3)
		FROM media_files
		WHERE content_id=$1 AND virtual_owner_installation_id=5`,
		contentID, barePath, candidatePath,
	).Scan(&bareCount, &candidateCount); err != nil {
		t.Fatalf("inspect files: %v", err)
	}

	if bareCount != 1 || candidateCount != 1 {
		t.Fatalf("expected bareCount=1 candidateCount=1, got bare=%d candidate=%d", bareCount, candidateCount)
	}
}

func TestReplaceVirtualResultPin_PostgresCAS(t *testing.T) {
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
	contentID := fmt.Sprintf("virtual-pin-cas-%d", suffix)
	deadPath := fmt.Sprintf("virtual://movie/tt%d?result=dead-candidate", suffix)
	livePath := fmt.Sprintf("virtual://movie/tt%d?result=live-winner", suffix)
	stalePath := fmt.Sprintf("virtual://movie/tt%d?result=stale-loser", suffix)

	var folderID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_folders(type,name,enabled)
		VALUES('movies',$1,true) RETURNING id`, fmt.Sprintf("Pin CAS %d", suffix)).Scan(&folderID); err != nil {
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
		VALUES($1,'movie','Pin CAS Item','matched','{}'::text[])`, contentID); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_item_libraries(content_id,media_folder_id)
		VALUES($1,$2)`, contentID, folderID); err != nil {
		t.Fatalf("seed item library: %v", err)
	}

	var fileID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_files(content_id,media_folder_id,file_path,file_size,container,virtual_owner_installation_id)
		VALUES($1,$2,$3,0,'mkv',5) RETURNING id`, contentID, folderID, deadPath).Scan(&fileID); err != nil {
		t.Fatalf("seed pinned virtual file: %v", err)
	}

	repo := NewFileRepository(pool)

	// 1. CAS with wrong expected path should return (false, nil) and not modify row
	replaced, err := repo.ReplaceVirtualResultPin(ctx, fileID, "virtual://movie/tt1234?result=wrong", livePath)
	if err != nil {
		t.Fatalf("ReplaceVirtualResultPin error on mismatch: %v", err)
	}
	if replaced {
		t.Fatal("ReplaceVirtualResultPin returned true on mismatched expectedPath")
	}

	var currentPath string
	if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE id=$1`, fileID).Scan(&currentPath); err != nil {
		t.Fatalf("fetch current path: %v", err)
	}
	if currentPath != deadPath {
		t.Fatalf("file path was modified on failed CAS: got %q, want %q", currentPath, deadPath)
	}

	// 2. CAS with matching expected path should return (true, nil) and update row
	replaced, err = repo.ReplaceVirtualResultPin(ctx, fileID, deadPath, livePath)
	if err != nil {
		t.Fatalf("ReplaceVirtualResultPin error on match: %v", err)
	}
	if !replaced {
		t.Fatal("ReplaceVirtualResultPin returned false on matching expectedPath")
	}

	if err := pool.QueryRow(ctx, `SELECT file_path FROM media_files WHERE id=$1`, fileID).Scan(&currentPath); err != nil {
		t.Fatalf("fetch current path: %v", err)
	}
	if currentPath != livePath {
		t.Fatalf("file path was not updated on successful CAS: got %q, want %q", currentPath, livePath)
	}

	// 3. Subsequent CAS with original deadPath should fail because row is now livePath
	replaced, err = repo.ReplaceVirtualResultPin(ctx, fileID, deadPath, stalePath)
	if err != nil {
		t.Fatalf("ReplaceVirtualResultPin error on stale: %v", err)
	}
	if replaced {
		t.Fatal("ReplaceVirtualResultPin returned true on stale expectedPath")
	}
}
