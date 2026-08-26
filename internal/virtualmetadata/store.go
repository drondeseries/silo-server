// Package virtualmetadata implements SiloDB: a content-keyed registry of
// provider-stream metadata. When a virtual stream is probed, its container,
// codecs, range and resolution are recorded against the content it belongs to.
// A later first play of a DIFFERENT provider stream for the same content can
// then pick a stream-copy remux route (or skip a provider probe) because the
// codec facts most variants of the content share are already known. Per-stream
// probe detail stays in media_files; this table carries only the aggregate
// routing signal.
package virtualmetadata

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Evidence is the content-keyed aggregate recorded from a successful probe.
type Evidence struct {
	ContentID  string
	Container  string
	CodecVideo string
	CodecAudio string
	VideoRange string
	Resolution string
}

// Store persists and reads content-keyed virtual stream metadata. It is
// best-effort: a read misses when nothing has been recorded for the content,
// and a write failure must never fail playback.
type Store interface {
	Get(ctx context.Context, contentID string) (Evidence, bool, error)
	Record(ctx context.Context, ev Evidence) error
}

// PostgresStore implements Store on top of the shared Postgres pool.
type PostgresStore struct {
	db *pgxpool.Pool
	// Now is injected for testability; it defaults to time.Now.
	Now func() time.Time
}

// NewPostgresStore returns a Store backed by the given pool.
func NewPostgresStore(db *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{db: db}
}

// Get returns the recorded evidence for the content, or (false, nil) when none.
func (s *PostgresStore) Get(ctx context.Context, contentID string) (Evidence, bool, error) {
	if contentID == "" {
		return Evidence{}, false, nil
	}
	var ev Evidence
	ev.ContentID = contentID
	err := s.db.QueryRow(ctx,
		`SELECT container, codec_video, codec_audio, video_range, resolution
		   FROM virtual_stream_metadata WHERE content_id = $1`, contentID,
	).Scan(&ev.Container, &ev.CodecVideo, &ev.CodecAudio, &ev.VideoRange, &ev.Resolution)
	if errors.Is(err, pgx.ErrNoRows) {
		return Evidence{}, false, nil
	}
	if err != nil {
		return Evidence{}, false, err
	}
	return ev, true, nil
}

// Record upserts a probe observation. Empty fields never overwrite a known-good
// value, and the probe count is incremented so the table reflects coverage.
func (s *PostgresStore) Record(ctx context.Context, ev Evidence) error {
	if ev.ContentID == "" {
		return nil
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO virtual_stream_metadata
			(content_id, container, codec_video, codec_audio, video_range, resolution, probe_count, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, 1, now())
		ON CONFLICT (content_id) DO UPDATE SET
			container   = CASE WHEN EXCLUDED.container   <> '' THEN EXCLUDED.container   ELSE virtual_stream_metadata.container   END,
			codec_video = CASE WHEN EXCLUDED.codec_video <> '' THEN EXCLUDED.codec_video ELSE virtual_stream_metadata.codec_video END,
			codec_audio = CASE WHEN EXCLUDED.codec_audio <> '' THEN EXCLUDED.codec_audio ELSE virtual_stream_metadata.codec_audio END,
			video_range = CASE WHEN EXCLUDED.video_range <> '' THEN EXCLUDED.video_range ELSE virtual_stream_metadata.video_range END,
			resolution  = CASE WHEN EXCLUDED.resolution  <> '' THEN EXCLUDED.resolution  ELSE virtual_stream_metadata.resolution  END,
			probe_count = virtual_stream_metadata.probe_count + 1,
			updated_at  = now()`,
		ev.ContentID, ev.Container, ev.CodecVideo, ev.CodecAudio, ev.VideoRange, ev.Resolution)
	return err
}

// Empty reports whether the evidence carries any routing signal at all.
func (ev Evidence) Empty() bool {
	return ev.Container == "" && ev.CodecVideo == "" && ev.CodecAudio == "" &&
		ev.VideoRange == "" && ev.Resolution == ""
}
