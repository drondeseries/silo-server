package playback

import (
	"strings"

	"github.com/Silo-Server/silo-server/internal/models"
)

// SameAudioRenditionSetV3 reports whether two rendition sets describe the same
// HLS generation's audio contract: both non-empty, equal length, and
// element-wise equal on the set-defining fields — Index, TrackID, Codec, and
// the Languages multiset. URL and Default are deliberately ignored: URL is
// session-derived, and Default legitimately moves when the viewer switches
// renditions. A genuine set change (a track disappeared, a codec changed, the
// language family changed) returns false and forces a transport rebuild.
func SameAudioRenditionSetV3(a, b []AudioRenditionV3) bool {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return false
	}
	for i := range a {
		left, right := a[i], b[i]
		if left.Index != right.Index ||
			left.TrackID != right.TrackID ||
			!strings.EqualFold(strings.TrimSpace(left.Codec), strings.TrimSpace(right.Codec)) ||
			!languageListsEqualV3(left.Languages, right.Languages) {
			return false
		}
	}
	return true
}

// languageListsEqualV3 compares two language-code lists as sorted multisets so
// container ordering and code spelling differences never produce a mismatch.
func languageListsEqualV3(a, b []string) bool {
	left := sortedLanguages(a)
	right := sortedLanguages(b)
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// audioRenditionsDeliveryConditionsV3 reports whether the source/client/recipe
// conditions for preferring the renditions-capable HLS remux delivery over
// progressive hold: the effective file carries at least two audio tracks, the
// client advertises HLS delivery, and the audio is deliverable by the
// renditions recipe (native copy, or AAC conversion within the HLS remux
// budget). The planner combines this with the AudioRenditionsEnabled switch,
// which is deliberately not consulted here so the predicate is testable while
// the activation flag is a compile-time constant.
func audioRenditionsDeliveryConditionsV3(file *models.MediaFile, hlsDeliveryOK, hlsTranscodeAudio, hlsAACAvailable bool) bool {
	return file != nil &&
		len(file.AudioTracks) >= 2 &&
		hlsDeliveryOK &&
		(!hlsTranscodeAudio || hlsAACAvailable)
}

// audioRenditionsDefaultTrackIndex returns the container-default audio track:
// the first track flagged Default, or index 0. The generation-level audio
// facts are frozen from this track so they never change with the viewer's
// rendition selection.
func audioRenditionsDefaultTrackIndex(file *models.MediaFile) int {
	if file == nil {
		return 0
	}
	for i, track := range file.AudioTracks {
		if track.Default {
			return i
		}
	}
	return 0
}

// FreezeRenditionsGenerationAudioV3 freezes the generation-level audio facts of
// a renditions plan to the container-default rendition's source facts (the
// default audio track's codec/channels/layout) instead of the selected track's.
// This mirrors the recipe's existing subtitle-identity exclusion: the plan's
// EffectiveRecipe audio fields, its Claims.Audio, and the executable recipe's
// source-channel facts must describe the SAME HLS generation regardless of
// which rendition the viewer has selected, so the byte-equivalence comparisons
// (sameExecutableAVRecipeV3 / sameEffectiveAVRecipeV3 / Claims.Audio equality)
// stay stable across a rendition switch. The plan's SelectedTracks.Audio still
// names the actual selection.
//
// DEAD PATH: no-ops while AudioRenditionsEnabled is false or the plan does not
// carry renditions; single-audio and progressive plans are untouched.
func FreezeRenditionsGenerationAudioV3(file *models.MediaFile, plan *PlanV3, request StartRequestV3, recipe *ExecutableRecipeV3) {
	if !AudioRenditionsEnabled {
		return
	}
	freezeRenditionsGenerationAudio(file, plan, request, recipe)
}

// freezeRenditionsGenerationAudio is the ungated core of
// FreezeRenditionsGenerationAudioV3; tests exercise it directly because the
// activation flag is a compile-time constant.
func freezeRenditionsGenerationAudio(file *models.MediaFile, plan *PlanV3, request StartRequestV3, recipe *ExecutableRecipeV3) {
	if file == nil || plan == nil || len(plan.AudioRenditions) == 0 || len(file.AudioTracks) == 0 {
		return
	}
	idx := audioRenditionsDefaultTrackIndex(file)
	if idx < 0 || idx >= len(file.AudioTracks) {
		return
	}
	track := file.AudioTracks[idx]
	codec := normalizeCodecV3(track.Codec)
	channels := track.Channels
	layout := normalizeLayoutV3(track.Layout)

	plan.EffectiveRecipe.AudioCodec = codec
	plan.EffectiveRecipe.AudioChannels = intPointerV3(channels)
	plan.EffectiveRecipe.AudioLayout = layout

	// Re-derive the audio claims from the default rendition's source facts so
	// the equality comparison is stable across a rendition switch.
	source := plan.Source
	source.AudioCodec = codec
	source.AudioChannels = channels
	source.AudioLayout = layout
	_, _, claims := audioEligibilityV3(source, request)
	plan.Claims.Audio = claims

	// The executable recipe freezes the source-channel facts that drive the
	// byte-equivalence comparison. Copy-audio recipes already freeze to a
	// stable zero (no re-encode); re-encoding recipes carry the default
	// rendition's channels.
	if recipe != nil && recipe.TranscodeAudio {
		recipe.SourceAudioChannels = channels
	}
}
