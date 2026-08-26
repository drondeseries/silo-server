package plugins

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestInstallationStoreUpdateReplacementCleansVirtualMediaAtomically(t *testing.T) {
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

	var installationID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO plugin_installations(plugin_id,version,install_path,enabled,update_policy)
		VALUES('test.virtual-replacement','0.0.1','/tmp/test-virtual-replacement',true,'manual')
		RETURNING id`).Scan(&installationID); err != nil {
		t.Fatalf("seed plugin installation: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM plugin_installations WHERE id=$1`, installationID)
	})

	const folderID = 969
	const contentID = "movie-test-virtual-replacement"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled)
		VALUES($1,'Atomic Replacement','movies',true);
		INSERT INTO media_items(
			content_id,type,title,virtual_owner_installation_id,virtual_source
		) VALUES($2,'movie','Atomic Replacement',$3,'test-source');
		INSERT INTO media_files(
			content_id,media_folder_id,file_path,file_size,container,
			probe_source,virtual_owner_installation_id
		) VALUES($2,$1,'virtual://movie/test-virtual-replacement',0,'virtual','virtual',$3);
		INSERT INTO virtual_media_source_claims(
			plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata
		) VALUES($3,'test-source',$2,$1,true);
		INSERT INTO virtual_media_file_source_claims(
			plugin_installation_id,source_key,content_id,media_folder_id,file_path
		) VALUES($3,'test-source',$2,$1,'virtual://movie/test-virtual-replacement')`,
		folderID, contentID, installationID); err != nil {
		t.Fatalf("seed virtual replacement catalog: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})

	version := "0.0.2"
	path := "/tmp/test-virtual-replacement-new"
	store := NewInstallationStore(pool)
	if err := store.Update(ctx, installationID, UpdateInstallationInput{
		Version:            &version,
		InstallPath:        &path,
		RemoveVirtualMedia: true,
	}); err != nil {
		t.Fatalf("update replacement installation: %v", err)
	}

	var gotVersion, gotPath string
	if err := pool.QueryRow(ctx, `SELECT version,install_path FROM plugin_installations WHERE id=$1`, installationID).Scan(&gotVersion, &gotPath); err != nil {
		t.Fatalf("inspect updated installation: %v", err)
	}
	if gotVersion != version || gotPath != path {
		t.Fatalf("updated installation version=%q path=%q, want %q/%q", gotVersion, gotPath, version, path)
	}

	var files, sourceClaims, fileClaims, items int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_files WHERE content_id=$1),
		  (SELECT count(*) FROM virtual_media_source_claims WHERE content_id=$1),
		  (SELECT count(*) FROM virtual_media_file_source_claims WHERE content_id=$1),
		  (SELECT count(*) FROM media_items WHERE content_id=$1)`, contentID,
	).Scan(&files, &sourceClaims, &fileClaims, &items); err != nil {
		t.Fatalf("inspect cleaned virtual replacement catalog: %v", err)
	}
	if files != 0 || sourceClaims != 0 || fileClaims != 0 || items != 0 {
		t.Fatalf("cleaned virtual replacement files=%d source_claims=%d file_claims=%d items=%d", files, sourceClaims, fileClaims, items)
	}
}

func TestInstallationStoreUpdateReplacementRollsBackVirtualCleanupOnFailure(t *testing.T) {
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

	var installationID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO plugin_installations(plugin_id,version,install_path,enabled,update_policy)
		VALUES('test.virtual-replacement-rollback','0.0.1','/tmp/test-virtual-replacement-rollback',true,'manual')
		RETURNING id`).Scan(&installationID); err != nil {
		t.Fatalf("seed plugin installation: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM plugin_installations WHERE id=$1`, installationID)
	})

	const folderID = 968
	const contentID = "movie-test-virtual-replacement-rollback"
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_folders(id,name,type,enabled)
		VALUES($1,'Atomic Replacement Rollback','movies',true);
		INSERT INTO media_items(
			content_id,type,title,virtual_owner_installation_id,virtual_source
		) VALUES($2,'movie','Atomic Replacement Rollback',$3,'test-source');
		INSERT INTO media_files(
			content_id,media_folder_id,file_path,file_size,container,
			probe_source,virtual_owner_installation_id
		) VALUES($2,$1,'virtual://movie/test-virtual-replacement-rollback',0,'virtual','virtual',$3);
		INSERT INTO virtual_media_source_claims(
			plugin_installation_id,source_key,content_id,media_folder_id,owns_item_metadata
		) VALUES($3,'test-source',$2,$1,true);
		INSERT INTO virtual_media_file_source_claims(
			plugin_installation_id,source_key,content_id,media_folder_id,file_path
		) VALUES($3,'test-source',$2,$1,'virtual://movie/test-virtual-replacement-rollback')`,
		folderID, contentID, installationID); err != nil {
		t.Fatalf("seed rollback virtual catalog: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM media_folders WHERE id=$1`, folderID)
	})

	version := "0.0.2"
	store := NewInstallationStore(pool)
	err = store.Update(ctx, installationID, UpdateInstallationInput{
		Version:            &version,
		RemoveVirtualMedia: true,
		Capabilities: []Capability{
			{Type: "test", ID: "duplicate"},
			{Type: "test", ID: "duplicate"},
		},
	})
	if err == nil {
		t.Fatal("replacement update with duplicate capabilities = nil, want error")
	}

	var gotVersion string
	if err := pool.QueryRow(ctx, `SELECT version FROM plugin_installations WHERE id=$1`, installationID).Scan(&gotVersion); err != nil {
		t.Fatalf("inspect rolled-back installation: %v", err)
	}
	if gotVersion != "0.0.1" {
		t.Fatalf("rolled-back installation version=%q, want 0.0.1", gotVersion)
	}

	var files, sourceClaims, fileClaims, items int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM media_files WHERE content_id=$1),
		  (SELECT count(*) FROM virtual_media_source_claims WHERE content_id=$1),
		  (SELECT count(*) FROM virtual_media_file_source_claims WHERE content_id=$1),
		  (SELECT count(*) FROM media_items WHERE content_id=$1)`, contentID,
	).Scan(&files, &sourceClaims, &fileClaims, &items); err != nil {
		t.Fatalf("inspect rolled-back virtual catalog: %v", err)
	}
	if files != 1 || sourceClaims != 1 || fileClaims != 1 || items != 1 {
		t.Fatalf("rolled-back virtual catalog files=%d source_claims=%d file_claims=%d items=%d, want 1/1/1/1", files, sourceClaims, fileClaims, items)
	}
}
