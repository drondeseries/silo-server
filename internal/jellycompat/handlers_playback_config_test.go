package jellycompat

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/config"
)

func TestPlaybackHandlerReadsLiveSegmentRetention(t *testing.T) {
	cfg := &config.Config{Playback: config.PlaybackConfig{SegmentRetentionSeconds: 300}}
	handler := NewPlaybackHandler(cfg, nil, nil, nil, nil, nil, nil, nil)

	if got := handler.tm.Config().SegmentRetentionSeconds; got != 300 {
		t.Fatalf("configured segment retention = %d, want 300", got)
	}
	liveRetention := 300
	handler.SegmentRetentionSeconds = func() int { return liveRetention }
	liveRetention = 120
	if got := handler.tm.Config().SegmentRetentionSeconds; got != 120 {
		t.Fatalf("reloaded segment retention = %d, want 120", got)
	}
}
