package handlers

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type virtualProbeResult struct {
	VideoCodec string
	AudioCodec string
	HDR        bool
}

type virtualProbeCacheEntry struct {
	result    virtualProbeResult
	expiresAt time.Time
	err       error
}

var virtualProbeCache = struct {
	sync.Mutex
	entries map[string]virtualProbeCacheEntry
}{entries: make(map[string]virtualProbeCacheEntry)}

func probeVirtualStream(ctx context.Context, ffmpegPath, streamURL string) (virtualProbeResult, error) {
	now := time.Now()
	virtualProbeCache.Lock()
	if entry, ok := virtualProbeCache.entries[streamURL]; ok && now.Before(entry.expiresAt) {
		virtualProbeCache.Unlock()
		return entry.result, entry.err
	}
	virtualProbeCache.Unlock()
	ffprobe := filepath.Join(filepath.Dir(ffmpegPath), "ffprobe")
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, ffprobe, "-v", "error", "-analyzeduration", "3000000", "-probesize", "5000000", "-show_entries", "stream=codec_type,codec_name,color_transfer,color_primaries", "-of", "json", streamURL)
	out, err := cmd.Output()
	if err != nil {
		virtualProbeCache.Lock()
		virtualProbeCache.entries[streamURL] = virtualProbeCacheEntry{expiresAt: now.Add(2 * time.Minute), err: err}
		virtualProbeCache.Unlock()
		return virtualProbeResult{}, err
	}
	var payload struct {
		Streams []struct {
			Type      string `json:"codec_type"`
			Codec     string `json:"codec_name"`
			Transfer  string `json:"color_transfer"`
			Primaries string `json:"color_primaries"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return virtualProbeResult{}, err
	}
	result := virtualProbeResult{}
	for _, stream := range payload.Streams {
		switch stream.Type {
		case "video":
			result.VideoCodec = strings.ToLower(stream.Codec)
			result.HDR = strings.Contains(strings.ToLower(stream.Transfer), "2084") || strings.Contains(strings.ToLower(stream.Transfer), "arib") || strings.Contains(strings.ToLower(stream.Primaries), "2020")
		case "audio":
			if result.AudioCodec == "" {
				result.AudioCodec = strings.ToLower(stream.Codec)
			}
		}
	}
	virtualProbeCache.Lock()
	virtualProbeCache.entries[streamURL] = virtualProbeCacheEntry{result: result, expiresAt: now.Add(6 * time.Hour)}
	virtualProbeCache.Unlock()
	return result, nil
}
