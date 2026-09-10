package remuxdb

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestBuildSubmissionMovie(t *testing.T) {
	file := &models.MediaFile{
		ContentID:   "movie-tmdb-12345",
		FilePath:    "virtual://movie/tt0012345",
		Duration:    7200,
		Container:   "mkv",
		FileSize:    5000000000,
		Bitrate:     5500,
		ReleaseName: "Movie.2024.1080p.mkv",
		VideoTracks: []models.VideoTrack{
			{Codec: "h264", Width: 1920, Height: 1080, FrameRate: "23.976"},
		},
		AudioTracks: []models.AudioTrack{
			{Codec: "aac", Channels: 6, Language: "eng", Default: true},
		},
	}
	payload, ok := BuildSubmission(file, "", "0123456789abcdef0123456789abcdef01234567", nil)
	if !ok {
		t.Fatal("expected build submission to succeed")
	}
	if payload.Kind != "movie" {
		t.Errorf("kind = %q, want movie", payload.Kind)
	}
	if payload.Filename != "Movie.2024.1080p.mkv" {
		t.Errorf("filename = %q", payload.Filename)
	}
	if payload.ExternalIDs == nil || payload.ExternalIDs.IMDbID != "tt0012345" || payload.ExternalIDs.TMDbID != 12345 {
		t.Errorf("external ids = %+v", payload.ExternalIDs)
	}
	if len(payload.Tracks) != 2 {
		t.Errorf("tracks = %d, want 2", len(payload.Tracks))
	}
	if payload.Tracks[0].Kind != "video" || payload.Tracks[0].Width != 1920 {
		t.Errorf("video track = %+v", payload.Tracks[0])
	}
}

func TestBuildSubmissionEpisodeWithNZB(t *testing.T) {
	file := &models.MediaFile{
		ContentID:     "series-tvdb-999",
		EpisodeID:     "ep-1",
		SeasonNumber:  2,
		EpisodeNumber: 5,
		FilePath:      "virtual://series/tt9999999/2/5",
		Duration:      3600,
		VideoTracks: []models.VideoTrack{
			{Codec: "hevc", Width: 3840, Height: 2160},
		},
	}
	nzb := &NzbSubmission{
		Indexer:     "nzbgeek",
		IndexerGUID: "guid123",
	}
	payload, ok := BuildSubmission(file, "show.s02e05.mkv", "", nzb)
	if !ok {
		t.Fatal("expected build submission to succeed")
	}
	if payload.Kind != "episode" {
		t.Errorf("kind = %q, want episode", payload.Kind)
	}
	if payload.Season == nil || *payload.Season != 2 || payload.Episode == nil || *payload.Episode != 5 {
		t.Errorf("season/episode = %v/%v", payload.Season, payload.Episode)
	}
	if payload.NZB == nil || payload.NZB.IndexerGUID != "guid123" {
		t.Errorf("nzb = %+v", payload.NZB)
	}
}

func TestBuildSubmissionRequiresIdentityAndVideo(t *testing.T) {
	if _, ok := BuildSubmission(nil, "", "abc", nil); ok {
		t.Fatal("nil file should fail")
	}
	empty := &models.MediaFile{Duration: 100}
	if _, ok := BuildSubmission(empty, "a.mkv", "abc", nil); ok {
		t.Fatal("file without video tracks should fail")
	}
	noIdent := &models.MediaFile{
		Duration:    100,
		VideoTracks: []models.VideoTrack{{Codec: "h264"}},
	}
	if _, ok := BuildSubmission(noIdent, "a.mkv", "", nil); ok {
		t.Fatal("file without hash or nzb should fail")
	}
}
