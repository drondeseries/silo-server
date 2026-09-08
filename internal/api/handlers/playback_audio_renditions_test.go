package handlers

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestAttachAudioRenditionsV3 verifies the renditions minting: one rendition
// per audio track with session-scoped playlist URLs, languages[] copied from
// the track, the selected track marked default, and track identities bound to
// the effective file.
func TestAttachAudioRenditionsV3(t *testing.T) {
	plan := &playback.PlanV3{PlanID: "plan:renditions"}
	file := &models.MediaFile{
		ID: 42,
		AudioTracks: []models.AudioTrack{
			{Language: "en", Languages: []string{"en", "fr", "de"}, Codec: "eac3"},
			{Language: "ja", Codec: "aac"},
		},
	}

	attachAudioRenditionsV3("sess-1", file, plan, 1)

	if len(plan.AudioRenditions) != 2 {
		t.Fatalf("AudioRenditions = %d, want 2", len(plan.AudioRenditions))
	}
	first := plan.AudioRenditions[0]
	if first.Index != 0 {
		t.Fatalf("first Index = %d, want 0", first.Index)
	}
	if first.TrackID != "file:42:audio:0" {
		t.Fatalf("first TrackID = %q, want file:42:audio:0", first.TrackID)
	}
	if first.Language != "en" {
		t.Fatalf("first Language = %q, want en", first.Language)
	}
	if !reflect.DeepEqual(first.Languages, []string{"en", "fr", "de"}) {
		t.Fatalf("first Languages = %v, want [en fr de]", first.Languages)
	}
	if first.Codec != "eac3" {
		t.Fatalf("first Codec = %q, want eac3", first.Codec)
	}
	if first.URL != "/playback/transcode/sess-1/audio_0/audio.m3u8" {
		t.Fatalf("first URL = %q", first.URL)
	}
	if first.Default {
		t.Fatalf("unselected track 0 must not be default")
	}
	second := plan.AudioRenditions[1]
	if second.Index != 1 || second.TrackID != "file:42:audio:1" || second.URL != "/playback/transcode/sess-1/audio_1/audio.m3u8" {
		t.Fatalf("second rendition = %#v", second)
	}
	if !second.Default {
		t.Fatalf("the selected track (index 1) must be marked default")
	}
}

// TestAttachAudioRenditionsV3DefaultsToIndexZero verifies the default mark
// falls back to index 0 when no specific track is selected, and that a file
// without audio tracks clears the list.
func TestAttachAudioRenditionsV3DefaultsToIndexZero(t *testing.T) {
	plan := &playback.PlanV3{}
	file := &models.MediaFile{
		ID: 7,
		AudioTracks: []models.AudioTrack{
			{Language: "en", Codec: "aac"},
			{Language: "fr", Codec: "aac"},
		},
	}
	attachAudioRenditionsV3("sess-2", file, plan, -1)
	if len(plan.AudioRenditions) != 2 || !plan.AudioRenditions[0].Default || plan.AudioRenditions[1].Default {
		t.Fatalf("default marking = %#v, want only index 0 default", plan.AudioRenditions)
	}

	empty := &playback.PlanV3{PlanID: "plan:empty"}
	attachAudioRenditionsV3("sess-2", &models.MediaFile{ID: 7}, empty, 0)
	if len(empty.AudioRenditions) != 0 {
		t.Fatalf("empty file produced renditions: %#v", empty.AudioRenditions)
	}
}

// TestAttachAudioRenditionsV3SessionScoping verifies every minted URL carries
// the session namespace (the same scoping the subtitle artifact URLs use).
func TestAttachAudioRenditionsV3SessionScoping(t *testing.T) {
	plan := &playback.PlanV3{}
	file := &models.MediaFile{
		ID: 9,
		AudioTracks: []models.AudioTrack{
			{Language: "de", Codec: "dts"},
		},
	}
	attachAudioRenditionsV3("sess-3", file, plan, 0)
	if len(plan.AudioRenditions) != 1 {
		t.Fatalf("AudioRenditions = %d, want 1", len(plan.AudioRenditions))
	}
	url := plan.AudioRenditions[0].URL
	if !strings.HasPrefix(url, "/playback/transcode/sess-3/audio_0/") {
		t.Fatalf("URL %q not scoped under the session namespace", url)
	}
}
