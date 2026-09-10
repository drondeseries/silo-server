package jellycompat

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// Mirror of the handlers probe-failure regression: a leading release marker
// must not leak into the audio inventory, and the merged tracks must be one
// per real language in declaration order — matching the native surface.
func TestMergeCompatCandidateTracksFiltersReleaseMarkersAndOrdersDeclaration(t *testing.T) {
	probed := &models.MediaFile{Resolution: "2160p", CodecVideo: "hevc"}
	candidate := VirtualPlaybackStream{
		CodecAudio:     "eac3",
		AudioLanguages: []string{"MULTI", "eng", "fre"},
	}

	mergeCompatCandidateTracks(probed, candidate)

	got := make([]string, 0, len(probed.AudioTracks))
	for _, track := range probed.AudioTracks {
		got = append(got, track.Language)
	}
	if len(got) != 2 {
		t.Fatalf("audio languages = %v, want exactly [eng fre] (no MULTI leak, no duplicates)", got)
	}
	if got[0] != "eng" || got[1] != "fre" {
		t.Fatalf("audio languages = %v, want declaration order [eng fre]", got)
	}
	for _, track := range probed.AudioTracks {
		if track.Language == "" {
			t.Fatalf("anonymous audio track leaked: %#v", probed.AudioTracks)
		}
	}
}
