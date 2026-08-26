package remuxdb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestExtractIMDbID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"movie-imdb-tt0133093", "tt0133093"},
		{"virtual://movie/tt30825738?profile=default", "tt30825738"},
		{"tt19035928", "tt19035928"},
		{"no-imdb-id", ""},
		{"tt12", ""},
	}
	for _, tt := range tests {
		got := ExtractIMDbID(tt.input)
		if got != tt.want {
			t.Errorf("ExtractIMDbID(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFetchProbeAndPopulateMediaFile(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/media/info" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("imdb_id") != "tt0133093" {
			t.Errorf("unexpected imdb_id: %s", r.URL.Query().Get("imdb_id"))
		}
		resp := []MediaInfo{{
			Container: "mkv",
			Duration:  8160,
			Bitrate:   60000000,
			Tracks: []TrackDetail{
				{Kind: "video", Codec: "hevc", Height: 2160, Width: 3840, FPS: 23.976, ColorTransfer: "smpte2084"},
				{Kind: "audio", Codec: "aac", Channels: 6, Language: "eng"},
				{Kind: "subtitle", Codec: "subrip", Language: "eng"},
			},
		}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "")
	infos, err := client.FetchProbe(context.Background(), "tt0133093", nil, nil)
	if err != nil {
		t.Fatalf("FetchProbe failed: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("len(infos) = %d, want 1", len(infos))
	}

	file := &models.MediaFile{}
	ok := PopulateMediaFileFromRemuxDB(infos[0], file)
	if !ok {
		t.Fatalf("PopulateMediaFileFromRemuxDB returned false")
	}
	if file.CodecVideo != "hevc" || file.Resolution != "2160p" || file.CodecAudio != "aac" || !file.HDR {
		t.Fatalf("populated file = %#v", file)
	}
	if len(file.VideoTracks) != 1 || len(file.AudioTracks) != 1 || len(file.SubtitleTracks) != 1 {
		t.Fatalf("tracks populated incorrectly: %#v", file)
	}
}

func TestSubmitMediaInfo(t *testing.T) {
	var submitted bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/mediainfo" && r.Method == http.MethodPost {
			submitted = true
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "token-123")
	file := &models.MediaFile{
		ContentID:   "movie-tt0133093",
		Container:   "mkv",
		CodecVideo:  "h264",
		Resolution:  "1080p",
		VideoTracks: []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}},
	}
	payload, ok := BuildMediaInfoPayload(file, "tt0133093")
	if !ok {
		t.Fatalf("BuildMediaInfoPayload returned false")
	}

	if err := client.SubmitMediaInfo(context.Background(), payload); err != nil {
		t.Fatalf("SubmitMediaInfo failed: %v", err)
	}
	if !submitted {
		t.Fatalf("server did not receive submission")
	}
}
