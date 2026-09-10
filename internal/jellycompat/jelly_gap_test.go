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

// Mirror of the native surface's hint-becomes-metadata fixture: once a real
// probed inventory exists, provider-only language hints must not become
// fabricated selectable audio tracks on the Jellyfin surface either.
func TestMergeCompatCandidateTracksHintBecomesMetadataNotSelectableStream(t *testing.T) {
	probed := &models.MediaFile{
		AudioTracks: []models.AudioTrack{
			{Index: 1, Language: "en", Codec: "aac", Channels: 2, Default: true},
		},
	}
	candidate := VirtualPlaybackStream{
		CodecAudio:     "aac",
		AudioLanguages: []string{"ENG", "FRA"},
	}

	mergeCompatCandidateTracks(probed, candidate)

	if len(probed.AudioTracks) != 1 {
		t.Fatalf("audio tracks = %#v, want exactly the probed English track (FRA is metadata, not a stream)", probed.AudioTracks)
	}
	foundFRA := false
	for _, lang := range probed.AudioTracks[0].Languages {
		if lang == "FRA" {
			foundFRA = true
		}
	}
	if !foundFRA {
		t.Fatalf("FRA hint missing from the probed track's Languages metadata: %#v", probed.AudioTracks[0].Languages)
	}
}
