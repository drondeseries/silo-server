package jellycompat

import (
	"context"
	"errors"
	"strings"

	"github.com/Silo-Server/silo-server/internal/models"
)

const virtualPlaybackPrefix = "virtual://"

type VirtualPlaybackResolver interface {
	ResolveVirtualPlayback(ctx context.Context, virtualPath string, userID int, profileID string) (string, error)
}

func isVirtualPlaybackFile(file *models.MediaFile) bool {
	return file != nil && strings.HasPrefix(file.FilePath, virtualPlaybackPrefix)
}

func (h *PlaybackHandler) resolveVirtualPlayback(ctx context.Context, file *models.MediaFile, userID int, profileID string) (string, error) {
	if !isVirtualPlaybackFile(file) {
		return "", nil
	}
	if h.VirtualPlaybackResolver == nil {
		return "", errors.New("virtual playback resolver is not configured")
	}
	return h.VirtualPlaybackResolver.ResolveVirtualPlayback(ctx, file.FilePath, userID, profileID)
}
