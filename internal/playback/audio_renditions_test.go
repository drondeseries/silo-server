package playback

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestSameAudioRenditionSetV3(t *testing.T) {
	base := []AudioRenditionV3{
		{Index: 0, TrackID: "file:1:audio:0", Language: "en", Languages: []string{"en", "fr"}, Codec: "eac3", URL: "/playback/transcode/s1/audio_0/audio.m3u8", Default: false},
		{Index: 1, TrackID: "file:1:audio:1", Language: "ja", Codec: "aac", URL: "/playback/transcode/s1/audio_1/audio.m3u8", Default: true},
	}

	// The same set after a rendition switch: URL is session-derived and the
	// Default marking moved to the newly-selected track — both must be ignored.
	switched := []AudioRenditionV3{
		{Index: 0, TrackID: "file:1:audio:0", Language: "en", Languages: []string{"fr", "en"}, Codec: "eac3", URL: "/playback/transcode/s1/audio_0/audio.m3u8", Default: true},
		{Index: 1, TrackID: "file:1:audio:1", Language: "ja", Codec: "aac", URL: "/playback/transcode/s1/audio_1/audio.m3u8", Default: false},
	}
	if !SameAudioRenditionSetV3(base, switched) {
		t.Fatal("equal rendition sets with URL/Default differences must compare equal")
	}
	if !SameAudioRenditionSetV3(base, base) {
		t.Fatal("identical rendition set must compare equal")
	}

	// A language removed from a track's multiset changes the set.
	removed := []AudioRenditionV3{
		{Index: 0, TrackID: "file:1:audio:0", Language: "en", Languages: []string{"en"}, Codec: "eac3"},
		{Index: 1, TrackID: "file:1:audio:1", Language: "ja", Codec: "aac"},
	}
	if SameAudioRenditionSetV3(base, removed) {
		t.Fatal("changed language multiset must compare unequal")
	}

	// A codec change changes the set.
	codec := []AudioRenditionV3{
		{Index: 0, TrackID: "file:1:audio:0", Language: "en", Languages: []string{"en", "fr"}, Codec: "ac3"},
		{Index: 1, TrackID: "file:1:audio:1", Language: "ja", Codec: "aac"},
	}
	if SameAudioRenditionSetV3(base, codec) {
		t.Fatal("changed codec must compare unequal")
	}

	// A disappeared track changes the set.
	shrunk := []AudioRenditionV3{
		{Index: 0, TrackID: "file:1:audio:0", Language: "en", Languages: []string{"en", "fr"}, Codec: "eac3"},
	}
	if SameAudioRenditionSetV3(base, shrunk) {
		t.Fatal("shrunk rendition set must compare unequal")
	}

	// Empty sets are never a generation's audio contract.
	if SameAudioRenditionSetV3(nil, nil) {
		t.Fatal("empty sets must not compare equal")
	}
	if SameAudioRenditionSetV3(base, nil) {
		t.Fatal("non-empty vs empty must not compare equal")
	}
}

func TestFreezeRenditionsGenerationAudioStableAcrossRenditionSwitch(t *testing.T) {
	file := &models.MediaFile{
		ID: 42,
		AudioTracks: []models.AudioTrack{
			{Language: "en", Codec: "eac3", Channels: 6, Layout: "5.1", Default: true},
			{Language: "ja", Codec: "aac", Channels: 2, Layout: "stereo"},
		},
	}
	request := validStartRequestV3()
	request.FileID = 42
	request.Capabilities.CodecsAudio = []string{"eac3", "aac"}

	renditions := []AudioRenditionV3{
		{Index: 0, TrackID: "file:42:audio:0", Language: "en", Codec: "eac3"},
		{Index: 1, TrackID: "file:42:audio:1", Language: "ja", Codec: "aac"},
	}

	// Two plans selecting different tracks, each with a pre-freeze effective
	// recipe describing ITS selection.
	buildPlan := func(selectedCodec string, selectedChannels int, selectedLayout string) *PlanV3 {
		plan := &PlanV3{
			Delivery:        DeliveryRemuxHLSV3,
			AudioRenditions: append([]AudioRenditionV3(nil), renditions...),
			EffectiveRecipe: EffectiveRecipeV3{
				AudioCodec:    normalizeCodecV3(selectedCodec),
				AudioChannels: intPointerV3(selectedChannels),
				AudioLayout:   normalizeLayoutV3(selectedLayout),
			},
			Source: SourceDescriptorV3{
				AudioCodec: normalizeCodecV3(selectedCodec), AudioChannels: selectedChannels, AudioLayout: normalizeLayoutV3(selectedLayout),
			},
		}
		recipe := &ExecutableRecipeV3{TranscodeAudio: false, TargetAudioCodec: "copy"}
		freezeRenditionsGenerationAudio(file, plan, request, recipe)
		return plan
	}
	selectedTrack0 := buildPlan("eac3", 6, "5.1")
	selectedTrack1 := buildPlan("aac", 2, "stereo")

	// Both plans now describe the container-default rendition (eac3 5.1), not
	// their respective selections, so the byte-equivalence comparisons are
	// stable across a rendition switch.
	if selectedTrack0.EffectiveRecipe.AudioCodec != "eac3" || selectedTrack1.EffectiveRecipe.AudioCodec != "eac3" {
		t.Fatalf("frozen AudioCodec = %q / %q, want eac3 for both", selectedTrack0.EffectiveRecipe.AudioCodec, selectedTrack1.EffectiveRecipe.AudioCodec)
	}
	if selectedTrack0.EffectiveRecipe.AudioChannels == nil || selectedTrack1.EffectiveRecipe.AudioChannels == nil ||
		*selectedTrack0.EffectiveRecipe.AudioChannels != 6 || *selectedTrack1.EffectiveRecipe.AudioChannels != 6 {
		t.Fatalf("frozen AudioChannels = %v / %v, want 6 for both", selectedTrack0.EffectiveRecipe.AudioChannels, selectedTrack1.EffectiveRecipe.AudioChannels)
	}
	if selectedTrack0.EffectiveRecipe.AudioLayout != "5.1" || selectedTrack1.EffectiveRecipe.AudioLayout != "5.1" {
		t.Fatalf("frozen AudioLayout = %q / %q, want 5.1 for both", selectedTrack0.EffectiveRecipe.AudioLayout, selectedTrack1.EffectiveRecipe.AudioLayout)
	}
	// Claims.Audio is identical across the switch and carries the default codec.
	if selectedTrack0.Claims.Audio != selectedTrack1.Claims.Audio {
		t.Fatalf("Claims.Audio diverged across a rendition switch: %#v vs %#v", selectedTrack0.Claims.Audio, selectedTrack1.Claims.Audio)
	}
	if selectedTrack0.Claims.Audio.Codec != "eac3" {
		t.Fatalf("Claims.Audio.Codec = %q, want eac3", selectedTrack0.Claims.Audio.Codec)
	}

	// A re-encoding recipe freezes SourceAudioChannels from the default track.
	transcodePlan := &PlanV3{
		Delivery:        DeliveryRemuxHLSV3,
		AudioRenditions: renditions,
	}
	transcodeRecipe := &ExecutableRecipeV3{TranscodeAudio: true, TargetAudioCodec: "aac", SourceAudioChannels: 2}
	freezeRenditionsGenerationAudio(file, transcodePlan, request, transcodeRecipe)
	if transcodeRecipe.SourceAudioChannels != 6 {
		t.Fatalf("frozen SourceAudioChannels = %d, want default track channels 6", transcodeRecipe.SourceAudioChannels)
	}

	// A plan without renditions (progressive or single-audio HLS) is untouched.
	plain := &PlanV3{Delivery: DeliveryRemuxProgressiveV3, EffectiveRecipe: EffectiveRecipeV3{AudioCodec: "aac", AudioChannels: intPointerV3(2), AudioLayout: "stereo"}}
	freezeRenditionsGenerationAudio(file, plain, request, nil)
	if plain.EffectiveRecipe.AudioCodec != "aac" {
		t.Fatalf("non-renditions plan was frozen: %q", plain.EffectiveRecipe.AudioCodec)
	}
}

func TestAudioRenditionsDeliveryConditions(t *testing.T) {
	multi := &models.MediaFile{AudioTracks: []models.AudioTrack{{}, {}}}
	single := &models.MediaFile{AudioTracks: []models.AudioTrack{{}}}

	if !audioRenditionsDeliveryConditionsV3(multi, true, false, false) {
		t.Fatal("multi-audio + HLS + copy audio must satisfy the conditions")
	}
	if !audioRenditionsDeliveryConditionsV3(multi, true, true, true) {
		t.Fatal("multi-audio + HLS + AAC toolchain available must satisfy the conditions")
	}
	if audioRenditionsDeliveryConditionsV3(multi, true, true, false) {
		t.Fatal("multi-audio + HLS without the AAC toolchain must not satisfy the conditions")
	}
	if audioRenditionsDeliveryConditionsV3(multi, false, false, false) {
		t.Fatal("multi-audio without HLS delivery must not satisfy the conditions")
	}
	if audioRenditionsDeliveryConditionsV3(single, true, false, false) {
		t.Fatal("single-audio must keep the current progressive-first ordering")
	}
	if audioRenditionsDeliveryConditionsV3(nil, true, false, false) {
		t.Fatal("nil file must not satisfy the conditions")
	}
	if AudioRenditionsEnabled {
		t.Skip("activation flag is on; the planner predicate is live and covered by integration tests")
	}
	// With the flag off the planner's full predicate is inert: nothing changes.
}
