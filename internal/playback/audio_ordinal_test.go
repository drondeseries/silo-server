package playback

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestAudioStreamOrdinalInterleavedAbsoluteIndexes verifies the shared
// conversion maps a selected track's array position to its rank among the
// audio tracks ordered by absolute container stream Index, with subtitles
// interleaved between the audio streams — the shape that breaks a naive
// array-position or absolute-index mapping.
func TestAudioStreamOrdinalInterleavedAbsoluteIndexes(t *testing.T) {
	tracks := []models.AudioTrack{
		{Index: 1, Language: "eng"},
		{Index: 3, Language: "fra"},
	}
	tests := []struct {
		name          string
		selectedIndex int
		want          int
	}{
		{name: "first audio at stream 1 maps to ordinal 0", selectedIndex: 0, want: 0},
		{name: "second audio at stream 3 maps to ordinal 1", selectedIndex: 1, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AudioStreamOrdinal(tracks, tt.selectedIndex); got != tt.want {
				t.Fatalf("AudioStreamOrdinal(tracks, %d) = %d, want %d", tt.selectedIndex, got, tt.want)
			}
		})
	}
}

// TestAudioStreamOrdinalSingleAudioAtStreamOne verifies a lone audio track at
// absolute stream 1 (video at 0) maps to ordinal 0.
func TestAudioStreamOrdinalSingleAudioAtStreamOne(t *testing.T) {
	tracks := []models.AudioTrack{{Index: 1, Language: "eng"}}
	if got := AudioStreamOrdinal(tracks, 0); got != 0 {
		t.Fatalf("AudioStreamOrdinal(tracks, 0) = %d, want 0", got)
	}
}

// TestAudioStreamOrdinalSynthesizedFallback verifies the legacy synthesized
// case: tracks without a usable Index (0 or absent — virtual sources) fall
// back to the array position as the ordinal.
func TestAudioStreamOrdinalSynthesizedFallback(t *testing.T) {
	tracks := []models.AudioTrack{
		{Language: "eng"},
		{Language: "fra"},
	}
	for _, tt := range []struct {
		selectedIndex int
		want          int
	}{
		{selectedIndex: 0, want: 0},
		{selectedIndex: 1, want: 1},
	} {
		if got := AudioStreamOrdinal(tracks, tt.selectedIndex); got != tt.want {
			t.Fatalf("AudioStreamOrdinal(tracks, %d) = %d, want %d", tt.selectedIndex, got, tt.want)
		}
	}
}

// TestAudioStreamOrdinalOutOfRangePassthrough verifies out-of-range selections
// pass through unchanged, matching the legacy behavior of the v3 conversion.
func TestAudioStreamOrdinalOutOfRangePassthrough(t *testing.T) {
	tracks := []models.AudioTrack{{Index: 1, Language: "eng"}}
	for _, selectedIndex := range []int{-1, 1, 5} {
		if got := AudioStreamOrdinal(tracks, selectedIndex); got != selectedIndex {
			t.Fatalf("AudioStreamOrdinal(tracks, %d) = %d, want passthrough %d", selectedIndex, got, selectedIndex)
		}
	}
}
