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

// renditionsReuseFixture builds the minimal record/candidate pair the reuse
// gate needs: a valid copy-audio HLS remux recipe, identical stream shape, and
// an HLS stream URL shared by both generations.
func renditionsReuseFixture(t *testing.T, planID string) (*playback.AttemptRecordV3, playback.PlanV3, playback.ExecutableRecipeV3) {
	t.Helper()
	recipe := playback.ExecutableRecipeV3{
		Version:             1,
		PlanID:              planID,
		PlayMethod:          playback.PlayRemux,
		SourceAudioChannels: 0,
	}
	stream := playback.StreamV3{
		URL:           "/playback/transcode/s1/master.m3u8",
		Protocol:      playback.StreamHLSV3,
		Container:     "hls",
		MIMEType:      "application/vnd.apple.mpegurl",
		Headers:       map[string]string{},
		HeaderRefresh: playback.HeaderRefreshNoneV3,
	}
	plan := playback.PlanV3{
		PlanID:         planID,
		Delivery:       playback.DeliveryRemuxHLSV3,
		Stream:         stream,
		SelectedTracks: playback.SelectedTracksV3{Audio: &playback.TrackIdentityV3{ID: "file:1:audio:0", Index: intPtr(0)}},
		AudioRenditions: []playback.AudioRenditionV3{
			{Index: 0, TrackID: "file:1:audio:0", Language: "en", Languages: []string{"en", "fr"}, Codec: "eac3"},
			{Index: 1, TrackID: "file:1:audio:1", Language: "ja", Codec: "aac"},
		},
	}
	record := &playback.AttemptRecordV3{
		CurrentPlanID:        planID,
		CurrentPlan:          plan,
		FrozenRecipe:         recipe,
		RequestedMediaFileID: 1,
		EffectiveMediaFileID: 1,
		NormalizedRequest:    playback.StartRequestV3{ClientPlaybackContext: playback.ClientPlaybackContextV3{Output: playback.OutputContextV3{OutputContextID: "route-1"}}},
	}
	return record, plan, recipe
}

// TestSidecarOnlyReuseReplanV3RenditionsAudioSwitch verifies a same-version
// AUDIO switch on renditions delivery reuses the active HLS generation when
// the rendition SET is unchanged, even though the selected audio identity
// moved. This is the behavior the client-side activation depends on.
func TestSidecarOnlyReuseReplanV3RenditionsAudioSwitch(t *testing.T) {
	record, currentPlan, recipe := renditionsReuseFixture(t, "plan:current")

	candidate := currentPlan
	candidate.PlanID = "plan:candidate"
	candidate.SelectedTracks.Audio = &playback.TrackIdentityV3{ID: "file:1:audio:1", Index: intPtr(1)}
	candidateRecipe := recipe
	candidateRecipe.PlanID = "plan:candidate"

	reusedRecipe, ok := sidecarOnlyReuseReplanV3(record, &candidate, candidateRecipe, "route-1")
	if !ok {
		t.Fatal("renditions audio switch with equal rendition sets must reuse the transport")
	}
	if reusedRecipe.PlanID != "plan:candidate" {
		t.Fatalf("reused recipe = %q, want the candidate recipe", reusedRecipe.PlanID)
	}
}

// TestSidecarOnlyReuseReplanV3RenditionsSetChangeRebuilds verifies a genuine
// rendition-set change (a track disappeared) still rebuilds the transport.
func TestSidecarOnlyReuseReplanV3RenditionsSetChangeRebuilds(t *testing.T) {
	record, currentPlan, recipe := renditionsReuseFixture(t, "plan:current")

	candidate := currentPlan
	candidate.PlanID = "plan:candidate"
	candidate.SelectedTracks.Audio = &playback.TrackIdentityV3{ID: "file:1:audio:1", Index: intPtr(1)}
	candidate.AudioRenditions = []playback.AudioRenditionV3{
		{Index: 0, TrackID: "file:1:audio:0", Language: "en", Languages: []string{"en", "fr"}, Codec: "eac3"},
	}
	candidateRecipe := recipe
	candidateRecipe.PlanID = "plan:candidate"

	if _, ok := sidecarOnlyReuseReplanV3(record, &candidate, candidateRecipe, "route-1"); ok {
		t.Fatal("a rendition-set change must rebuild the transport")
	}
}

// TestSidecarOnlyReuseReplanV3ProgressiveAudioSwitchRebuilds verifies a
// non-renditions audio switch (progressive delivery) keeps the historical
// rebuild behavior: byte-identical to before the renditions relaxation.
func TestSidecarOnlyReuseReplanV3ProgressiveAudioSwitchRebuilds(t *testing.T) {
	record, currentPlan, recipe := renditionsReuseFixture(t, "plan:current")
	// Strip renditions and switch to progressive: the generation has no
	// rendition contract, so an audio identity change must rebuild.
	record.CurrentPlan.Delivery = playback.DeliveryRemuxProgressiveV3
	currentPlan.Delivery = playback.DeliveryRemuxProgressiveV3
	currentPlan.AudioRenditions = nil

	candidate := currentPlan
	candidate.PlanID = "plan:candidate"
	candidate.SelectedTracks.Audio = &playback.TrackIdentityV3{ID: "file:1:audio:1", Index: intPtr(1)}
	candidateRecipe := recipe
	candidateRecipe.PlanID = "plan:candidate"

	if _, ok := sidecarOnlyReuseReplanV3(record, &candidate, candidateRecipe, "route-1"); ok {
		t.Fatal("a progressive audio switch must rebuild the transport (unchanged behavior)")
	}
}
