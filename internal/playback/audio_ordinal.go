package playback

import "github.com/Silo-Server/silo-server/internal/models"

// AudioStreamOrdinal translates a selected audio track's array position in
// tracks to the audio-only stream ordinal ffmpeg's `0:a:N` expects.
//
// The two index domains are distinct and must not be mixed:
//   - tracks[i].Index is the ABSOLUTE container stream index recorded by the
//     probe (video at stream 0, the only audio at stream 1, subtitles
//     interleaved between audio streams).
//   - ffmpeg's `0:a:N` counts only audio streams: 0 is the first audio stream
//     regardless of its absolute container index.
//
// Mapping by array position alone would select the wrong language when the
// track list order differs from the container order (MULTi releases, virtual
// sources), and mapping by the absolute Index would emit a specifier that can
// silently omit audio entirely (the optional `?` map) or pick a different
// language. The ordinal is therefore the rank of the selected track among the
// file's audio tracks ordered by their absolute stream Index (audio tracks
// with Index 1 and 3 → ordinals 0 and 1). When the track carries no usable
// Index (0 or absent — the legacy synthesized case), the array position is
// used as the ordinal, preserving the historical behavior for tracks without
// a recorded stream index. Out-of-range selections pass through unchanged.
func AudioStreamOrdinal(tracks []models.AudioTrack, selectedIndex int) int {
	if selectedIndex < 0 || selectedIndex >= len(tracks) {
		return selectedIndex
	}
	selected := tracks[selectedIndex]
	if selected.Index <= 0 {
		// Synthesized track (virtual sources): no real container stream backs
		// it, so the array position is the ordinal.
		return selectedIndex
	}
	ordinal := 0
	for _, track := range tracks {
		if track.Index > 0 && track.Index < selected.Index {
			ordinal++
		}
	}
	return ordinal
}
