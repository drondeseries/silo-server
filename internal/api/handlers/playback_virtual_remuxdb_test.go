package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/remuxdb"
)

func remuxTestVariants() []remuxdb.MediaInfo {
	return []remuxdb.MediaInfo{
		{
			Size:      2670000000,
			Container: "mkv",
			Tracks: []remuxdb.TrackDetail{
				{Kind: "video", Codec: "av1", Width: 1920, Height: 1080},
			},
		},
		{
			Size:      28979107000,
			Container: "mkv",
			Sources: []remuxdb.ProbeSource{
				{Filename: "named.movie.2024.1080p.mkv"},
			},
			Tracks: []remuxdb.TrackDetail{
				{Kind: "video", Codec: "h264", Width: 1920, Height: 1080},
				{Kind: "audio", Codec: "dts", Channels: 6},
			},
		},
	}
}

func remuxTestServer(t *testing.T, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		_ = json.NewEncoder(w).Encode(remuxTestVariants())
	}))
}

func TestMatchRemuxDBCandidatesRecordsSizeMatch(t *testing.T) {
	calls := 0
	ts := remuxTestServer(t, &calls)
	defer ts.Close()

	h := &PlaybackHandler{
		RemuxDBConfig: func(context.Context) remuxdb.Config {
			return remuxdb.Config{Enabled: true, BaseURL: ts.URL}
		},
	}
	file := &models.MediaFile{ContentID: "movie-tmdb-1", FilePath: "virtual://movie/tt0000001", MediaFolderID: 3}
	candidates := []VirtualPlaybackStream{
		{URI: "virtual://movie/tt0000001?result=a", FileSize: 28980000000, Resolution: "1080p", CodecVideo: "h264"},
		{URI: "virtual://movie/tt0000001?result=b", FileSize: 2670000000, Resolution: "1080p", CodecVideo: "av1"},
	}
	matched := h.matchRemuxDBCandidates(context.Background(), file, candidates)
	if calls != 1 {
		t.Fatalf("fetches = %d, want 1 shared fetch", calls)
	}
	if len(matched) != 2 {
		t.Fatalf("matched = %d releases, want 2", len(matched))
	}
	ev, ok := matched["virtual://movie/tt0000001?result=a"]
	if !ok || string(ev.MatchMethod) != string(remuxdb.MatchSizeTags) || ev.CodecVideo != "h264" {
		t.Fatalf("candidate a evidence = %+v %v", ev, ok)
	}

	backfilled := applyRemuxDBEvidence(file, matched, "virtual://movie/tt0000001?result=a")
	if backfilled == file || backfilled.CodecVideo != "h264" || backfilled.Resolution != "1080p" {
		t.Fatalf("backfilled = %+v same=%v", backfilled, backfilled == file)
	}
	if file.CodecVideo != "" {
		t.Fatal("source file mutated")
	}
}

func TestMatchRemuxDBCandidatesDisabled(t *testing.T) {
	calls := 0
	ts := remuxTestServer(t, &calls)
	defer ts.Close()

	h := &PlaybackHandler{
		RemuxDBConfig: func(context.Context) remuxdb.Config {
			return remuxdb.Config{Enabled: false, BaseURL: ts.URL}
		},
	}
	file := &models.MediaFile{ContentID: "movie-tmdb-1", FilePath: "virtual://movie/tt0000001"}
	matched := h.matchRemuxDBCandidates(context.Background(), file, []VirtualPlaybackStream{
		{URI: "virtual://movie/tt0000001?result=a", FileSize: 1},
	})
	if len(matched) != 0 || calls != 0 {
		t.Fatalf("disabled matched=%d calls=%d, want 0/0", len(matched), calls)
	}
}

func TestMatchRemuxDBCandidatesAbstainsWithoutIdentity(t *testing.T) {
	calls := 0
	ts := remuxTestServer(t, &calls)
	defer ts.Close()

	h := &PlaybackHandler{
		RemuxDBConfig: func(context.Context) remuxdb.Config {
			return remuxdb.Config{Enabled: true, BaseURL: ts.URL}
		},
	}
	file := &models.MediaFile{ContentID: "movie-tmdb-1", FilePath: "virtual://movie/tt0000001"}
	matched := h.matchRemuxDBCandidates(context.Background(), file, []VirtualPlaybackStream{
		{URI: "virtual://movie/tt0000001?result=a"},
	})
	if len(matched) != 0 {
		t.Fatalf("identity-free candidate matched: %+v", matched)
	}
}

func TestRemuxHintHDRRules(t *testing.T) {
	file := &models.MediaFile{}
	cand := VirtualPlaybackStream{}
	if hint := remuxHintForCandidate(file, cand); hint.HDRKnown {
		t.Fatalf("empty candidate hint marks HDR known: %+v", hint)
	}
	probed := &models.MediaFile{HDR: true, VideoTracks: []models.VideoTrack{{Codec: "hevc"}}}
	if hint := remuxHintForCandidate(probed, cand); !hint.HDRKnown || !hint.HDR {
		t.Fatalf("probed HDR hint = %+v, want known true", hint)
	}
	provider := VirtualPlaybackStream{HDR: "Dolby Vision"}
	if hint := remuxHintForCandidate(file, provider); !hint.HDRKnown || !hint.HDR {
		t.Fatalf("provider HDR hint = %+v, want known true", hint)
	}
}

func TestMatchRemuxDBCandidatesMatchesFilenameFromLabelWhenSizeZero(t *testing.T) {
	calls := 0
	ts := remuxTestServer(t, &calls)
	defer ts.Close()

	h := &PlaybackHandler{
		RemuxDBConfig: func(context.Context) remuxdb.Config {
			return remuxdb.Config{Enabled: true, BaseURL: ts.URL}
		},
	}
	file := &models.MediaFile{ContentID: "movie-tmdb-1", FilePath: "virtual://movie/tt0000001"}
	candidates := []VirtualPlaybackStream{
		{URI: "virtual://movie/tt0000001?result=named", Label: "named.movie.2024.1080p.mkv", FileSize: 0},
	}
	matched := h.matchRemuxDBCandidates(context.Background(), file, candidates)
	if len(matched) != 1 {
		t.Fatalf("matched = %d releases, want 1", len(matched))
	}
	ev, ok := matched["virtual://movie/tt0000001?result=named"]
	if !ok || string(ev.MatchMethod) != string(remuxdb.MatchFilename) || ev.CodecVideo != "h264" {
		t.Fatalf("candidate named evidence = %+v %v", ev, ok)
	}
}

func TestRemuxEvidenceDoesNotLeakToAlternativeCandidates(t *testing.T) {
	matched := map[string]remuxdb.Evidence{
		"virtual://movie/tt0000001?result=candA": {
			CodecVideo:  "hevc",
			VideoTracks: []remuxdb.TrackDetail{{Kind: "video", Codec: "hevc", Width: 3840, Height: 2160}},
		},
	}
	baseFile := &models.MediaFile{
		ContentID: "movie-tmdb-1",
		FilePath:  "virtual://movie/tt0000001",
	}

	candB := "virtual://movie/tt0000001?result=candB"
	backfilledB := applyRemuxDBEvidence(baseFile, matched, candB)
	if len(backfilledB.VideoTracks) != 0 || backfilledB.CodecVideo != "" {
		t.Fatalf("candidate B inherited candidate A's RemuxDB metadata: %+v", backfilledB)
	}

	candA := "virtual://movie/tt0000001?result=candA"
	backfilledA := applyRemuxDBEvidence(baseFile, matched, candA)
	if len(backfilledA.VideoTracks) == 0 || backfilledA.CodecVideo != "hevc" {
		t.Fatalf("candidate A failed to backfill: %+v", backfilledA)
	}
}

func TestRemuxHintDoesNotBorrowDifferentReleaseIdentity(t *testing.T) {
	file := &models.MediaFile{
		FilePath:    "virtual://movie/tt0000001?result=pinnedA",
		FileSize:    5000000000,
		ReleaseName: "Pinned.Movie.2024.2160p.mkv",
		Resolution:  "2160p",
		CodecVideo:  "hevc",
	}
	candB := VirtualPlaybackStream{
		URI: "virtual://movie/tt0000001?result=unrelatedB",
		ID:  "unrelatedB",
	}
	hintB := remuxHintForCandidate(file, candB)
	if hintB.Size != 0 || hintB.Filename != "" || hintB.Resolution != "" || hintB.CodecVideo != "" {
		t.Fatalf("candidate B borrowed release A's identity: %+v", hintB)
	}

	candA := VirtualPlaybackStream{
		URI: "virtual://movie/tt0000001?result=pinnedA",
		ID:  "pinnedA",
	}
	hintA := remuxHintForCandidate(file, candA)
	if hintA.Size != 5000000000 || hintA.Filename != "Pinned.Movie.2024.2160p.mkv" || hintA.Resolution != "2160p" || hintA.CodecVideo != "hevc" {
		t.Fatalf("candidate A failed to borrow its own file identity: %+v", hintA)
	}
}
