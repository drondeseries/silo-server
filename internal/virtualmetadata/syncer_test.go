package virtualmetadata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/remuxdb"
)

type fakeStore struct {
	recorded []Evidence
}

func (f *fakeStore) Get(_ context.Context, _ string) (Evidence, bool, error) {
	return Evidence{}, false, nil
}

func (f *fakeStore) Record(_ context.Context, ev Evidence) error {
	f.recorded = append(f.recorded, ev)
	return nil
}

func TestSyncRemuxDBOnStartupNilGuard(t *testing.T) {
	// Must not panic on nil inputs
	SyncRemuxDBOnStartup(context.Background(), nil, nil, nil)
}

func TestSyncRemuxDBFetchAndRecord(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("imdb_id") == "tt0133093" {
			_ = json.NewEncoder(w).Encode([]remuxdb.MediaInfo{{
				Container: "mkv",
				Tracks: []remuxdb.TrackDetail{
					{Kind: "video", Codec: "h264", Height: 1080, Width: 1920},
					{Kind: "audio", Codec: "aac", Channels: 2},
				},
			}})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	client := remuxdb.NewClient(ts.URL, "")
	store := &fakeStore{}

	// Directly test single item fetch logic
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	infos, err := client.FetchProbe(ctx, "tt0133093", nil, nil)
	if err != nil || len(infos) == 0 {
		t.Fatalf("fetch probe failed: %v", err)
	}

	ev := Evidence{
		ContentID:  "movie-imdb-tt0133093",
		Container:  infos[0].Container,
		CodecVideo: infos[0].Tracks[0].Codec,
		CodecAudio: infos[0].Tracks[1].Codec,
		Resolution: "1080p",
	}
	if err := store.Record(ctx, ev); err != nil {
		t.Fatalf("record failed: %v", err)
	}

	if len(store.recorded) != 1 || store.recorded[0].CodecVideo != "h264" {
		t.Fatalf("unexpected recorded evidence: %#v", store.recorded)
	}
}
