package remuxdb

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestStoreRoundTrip(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	variant := videoVariant(28979107000, "h264", 1080, "", 0,
		ProbeSource{Kind: "nzb", Filename: "release.mkv"})
	variant.Container = "mkv"
	variant.Duration = 6583.0
	variant.Bitrate = 35213589
	ev := EvidenceFromVariant("movie-1", "", 7, "virtual://movie/tt1", MatchSizeTags, &variant)

	if err := store.Record(ctx, ev); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, ok, err := store.Get(ctx, "movie-1", "", 7, "virtual://movie/tt1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok {
		t.Fatal("stored evidence not found")
	}
	if string(got.MatchMethod) != string(MatchSizeTags) || got.MatchedSize != 28979107000 {
		t.Fatalf("method/size = %q/%d, want size_tags/28979107000", got.MatchMethod, got.MatchedSize)
	}
	if got.CodecVideo != "h264" || got.Resolution != "1080p" || got.Container != "mkv" {
		t.Fatalf("evidence = %+v, want h264/1080p/mkv", got)
	}
	if len(got.VideoTracks) != 1 || len(got.AudioTracks) != 1 {
		t.Fatalf("tracks = %d video %d audio, want 1/1", len(got.VideoTracks), len(got.AudioTracks))
	}
	if !got.HDRKnown || got.HDR {
		t.Fatalf("hdr = %v known %v, want false/true", got.HDR, got.HDRKnown)
	}

	if _, ok, err := store.Get(ctx, "movie-1", "", 7, "virtual://movie/other"); err != nil || ok {
		t.Fatalf("unknown candidate get = %v %v, want miss", ok, err)
	}

	ev.MatchMethod = MatchInfoHash
	if err := store.Record(ctx, ev); err != nil {
		t.Fatalf("re-record: %v", err)
	}
	got, ok, err = store.Get(ctx, "movie-1", "", 7, "virtual://movie/tt1")
	if err != nil || !ok || string(got.MatchMethod) != string(MatchInfoHash) {
		t.Fatalf("updated evidence = %+v %v %v, want info_hash", got, ok, err)
	}
}

func TestStoreNilPoolIsNoop(t *testing.T) {
	var store *Store
	if _, ok, err := store.Get(context.Background(), "a", "", 0, "u"); err != nil || ok {
		t.Fatalf("nil get = %v %v", ok, err)
	}
	if err := store.Record(context.Background(), Evidence{}); err != nil {
		t.Fatalf("nil record: %v", err)
	}
	if err := NewStore(nil).Record(context.Background(), Evidence{}); err != nil {
		t.Fatalf("nil pool record: %v", err)
	}
}

func TestDecodeTrackListPropagatesCorruptJSON(t *testing.T) {
	var tracks []TrackDetail
	err := decodeTrackList([]byte(`{"not":"an array"}`), &tracks, "video tracks")
	if err == nil {
		t.Fatal("corrupt track JSON should return an error")
	}
	if !strings.Contains(err.Error(), "video tracks") {
		t.Fatalf("error = %v, want label video tracks", err)
	}
	if err := decodeTrackList(nil, &tracks, "video tracks"); err != nil {
		t.Fatalf("empty payload should be tolerated: %v", err)
	}
	if err := decodeTrackList([]byte(`[{"kind":"audio","idx":1}]`), &tracks, "audio tracks"); err != nil {
		t.Fatalf("valid payload: %v", err)
	}
	if len(tracks) != 1 || tracks[0].Index != 1 {
		t.Fatalf("tracks = %+v, want one index 1", tracks)
	}
}
