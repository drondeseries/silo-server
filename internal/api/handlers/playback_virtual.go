package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

const virtualPlaybackPrefix = "virtual://"

type VirtualPlaybackResolver interface {
	ResolveVirtualPlayback(ctx context.Context, virtualPath string, userID int, profileID string) (string, error)
}

func isVirtualPlaybackFile(file *models.MediaFile) bool {
	return file != nil && strings.HasPrefix(file.FilePath, virtualPlaybackPrefix)
}

func (h *PlaybackHandler) resolveVirtualPlayback(r *http.Request, file *models.MediaFile, profileID string) (string, error) {
	if !isVirtualPlaybackFile(file) {
		return "", nil
	}
	if h.VirtualPlaybackResolver == nil {
		return "", errors.New("virtual playback resolver is not configured")
	}
	return h.VirtualPlaybackResolver.ResolveVirtualPlayback(r.Context(), file.FilePath, apimw.GetUserID(r.Context()), profileID)
}

func (h *PlaybackHandler) startVirtualPlaybackV3(r *http.Request, req playback.StartRequestV3, requestDigest string, file *models.MediaFile, streamURL string) (playback.DecisionResponseV3, *transportErrorV3) {
	userID := apimw.GetUserID(r.Context())
	profileID := apimw.GetProfileID(r.Context())
	ctx := playback.WithClientInfo(r.Context(), playbackClientInfoFromRequest(r))
	audioTranscode := virtualAudioNeedsTranscode(req) && h.playbackConfig().FFmpegPath != ""
	var session *playback.Session
	var err error
	playMethod := playback.PlayDirect
	if audioTranscode {
		playMethod = playback.PlayRemux
	}
	if starter, ok := h.sessionMgr.(sessionStarterWithFilesContext); ok {
		session, err = starter.StartSessionWithFilesContext(ctx, userID, profileID, file.ID, file.ID, playMethod, audioTranscode)
	} else {
		session, err = h.sessionMgr.StartSessionWithFiles(userID, profileID, file.ID, file.ID, playMethod, audioTranscode)
	}
	planHash := sha256.Sum256([]byte(req.PlaybackAttemptID + "\x00" + file.FilePath))
	planID := "virtual:" + hex.EncodeToString(planHash[:8])
	if err != nil {
		return playback.DecisionResponseV3{}, sessionStartErrorV3(err)
	}
	abort := func() { _ = h.stopPlaybackSessionByID(context.WithoutCancel(r.Context()), session.ID, false) }
	position := floatOrZeroHandlerV3(req.StartPosition)
	if err := h.sessionMgr.UpdateProgress(session.ID, position, false); err != nil {
		abort()
		return playback.DecisionResponseV3{}, &transportErrorV3{reason: "internal_error", message: "Failed to initialize virtual playback.", cause: err}
	}
	// Plugin-backed streams do not have local probe metadata. When the client
	// does not advertise E-AC-3/AC-3 support, route the remote stream through
	// the local HLS transport and convert only the audio to AAC while copying
	// the video bitstream. This keeps virtual playback compatible with browser
	// and mobile decoders without forcing a video transcode.
	if audioTranscode {
		transportFile := *file
		transportFile.FilePath = streamURL
		transportFile.Container = "mkv"
		transportFile.CodecVideo = "copy"
		transportFile.Duration = 0
		audioChannels := 2
		plan := playback.PlanV3{
			ProtocolVersion:      playback.ProtocolV3,
			PlanID:               planID,
			SessionID:            session.ID,
			ExpiresAt:            playback.NewPlanExpiryV3(time.Now()),
			Delivery:             playback.DeliveryRemuxHLSV3,
			Engine:               playback.EngineMedia3HLSV3,
			DecisionReason:       "virtual_audio_transcode",
			RequestedMediaFileID: file.ID,
			EffectiveMediaFileID: file.ID,
			Source:               playback.SourceDescriptorV3{MediaFileID: file.ID},
			Stream:               playback.StreamV3{Protocol: playback.StreamHLSV3, Container: "hls", MIMEType: "application/vnd.apple.mpegurl", Headers: map[string]string{}, HeaderRefresh: playback.HeaderRefreshSessionV3},
			Timeline:             playback.TimelineV3{SourceStartSeconds: position, PlayerStartSeconds: position, CanSeekAnywhere: true, SeekRestoration: "player_position"},
			EffectiveRecipe:      playback.EffectiveRecipeV3{VideoCodec: "copy", AudioCodec: "aac", AudioChannels: &audioChannels, AudioLayout: "stereo"},
			Claims:               playback.ValidationClaimsV3{Audio: playback.AudioClaimsV3{Codec: "aac", Reason: "server_audio_adaptation"}},
			Transformations:      []playback.TransformationV3{{Name: "audio_to_aac", Executor: "server", RecipeVersion: "1", ValidatedClaims: []string{"media3_audio_decode"}}},
			RuntimeCorrections:   []string{}, DegradationWarnings: []playback.DegradationWarningV3{}, AppliedQuirks: []playback.AppliedQuirkV3{},
		}
		result := playback.PlannerResultV3{Plan: &plan, PlayMethod: playback.PlayRemux, TranscodeAudio: true, TargetVideoCodec: "copy", TargetAudioCodec: "aac", TargetAudioChannels: 2}
		prepared, prepareErr := h.prepareLocalTransportV3(r, session, &transportFile, result)
		if prepareErr != nil {
			abort()
			return playback.DecisionResponseV3{}, prepareErr
		}
		plan.Stream.URL = prepared.url
		record := playback.AttemptRecordV3{PlaybackAttemptID: req.PlaybackAttemptID, SessionID: session.ID, UserID: userID, ProfileID: profileID, RequestedMediaFileID: file.ID, EffectiveMediaFileID: file.ID, CurrentPlanID: planID, CurrentPlan: plan, NormalizedRequest: req, RequestDigest: requestDigest, ExpiresAt: time.Now().Add(playback.MaxTokenTTL)}
		if err := h.PlanStoreV3.SaveAttempt(r.Context(), record); err != nil {
			prepared.rollback()
			abort()
			return playback.DecisionResponseV3{}, &transportErrorV3{reason: "internal_error", message: "Failed to save virtual playback plan.", cause: err}
		}
		prepared.commit()
		h.syncSessionsNow(r.Context(), "v3_virtual_audio_transcode")
		return playback.DecisionResponseV3{ProtocolVersion: playback.ProtocolV3, ServerFeatures: []string{playback.FeaturePlaybackPlanV3, playback.FeatureMedia3Only}, Outcome: playback.OutcomePlayableV3, SessionID: session.ID, PlaybackPlan: &plan}, nil
	}
	effectiveStreamURL := streamURL

	plan := playback.PlanV3{
		ProtocolVersion:      playback.ProtocolV3,
		PlanID:               planID,
		SessionID:            session.ID,
		ExpiresAt:            playback.NewPlanExpiryV3(time.Now()),
		Delivery:             playback.DeliveryOriginalHTTPV3,
		Engine:               playback.EngineMedia3DirectV3,
		DecisionReason:       "virtual_playback_resolver",
		RequestedMediaFileID: file.ID,
		EffectiveMediaFileID: file.ID,
		Source:               playback.SourceDescriptorV3{MediaFileID: file.ID},
		Stream:               playback.StreamV3{Protocol: playback.StreamHTTPProgressiveV3, URL: effectiveStreamURL, Headers: map[string]string{}, HeaderRefresh: playback.HeaderRefreshSessionV3},
		Timeline:             playback.TimelineV3{SourceStartSeconds: position, PlayerStartSeconds: position, CanSeekAnywhere: true, SeekRestoration: "player_position"},
		Transformations:      []playback.TransformationV3{},
		AppliedQuirks:        []playback.AppliedQuirkV3{},
		RuntimeCorrections:   []string{},
		DegradationWarnings:  []playback.DegradationWarningV3{},
	}
	record := playback.AttemptRecordV3{
		PlaybackAttemptID: req.PlaybackAttemptID, SessionID: session.ID,
		UserID: userID, ProfileID: profileID,
		RequestedMediaFileID: file.ID, EffectiveMediaFileID: file.ID,
		CurrentPlanID: planID, CurrentPlan: plan, NormalizedRequest: req,
		RequestDigest: requestDigest, ExpiresAt: time.Now().Add(playback.MaxTokenTTL),
	}
	if err := h.PlanStoreV3.SaveAttempt(r.Context(), record); err != nil {
		abort()
		return playback.DecisionResponseV3{}, &transportErrorV3{reason: "internal_error", message: "Failed to save virtual playback plan.", cause: err}
	}
	h.syncSessionsNow(r.Context(), "v3_virtual_start")
	return playback.DecisionResponseV3{
		ProtocolVersion: playback.ProtocolV3,
		ServerFeatures:  []string{playback.FeaturePlaybackPlanV3, playback.FeatureMedia3Only},
		Outcome:         playback.OutcomePlayableV3,
		SessionID:       session.ID,
		PlaybackPlan:    &plan,
	}, nil
}

func virtualAudioNeedsTranscode(req playback.StartRequestV3) bool {
	for _, codec := range req.Capabilities.CodecsAudio {
		switch strings.ToLower(strings.TrimSpace(codec)) {
		case "eac3", "ac3", "aac", "mp3", "opus", "flac":
			// The provider's audio codec is unknown until the stream is opened.
			// Clients advertising E-AC-3/AC-3 can safely receive those common
			// surround tracks directly; other clients use the AAC compatibility
			// route. AAC-only clients still get adaptation for E-AC-3 sources.
			if strings.EqualFold(strings.TrimSpace(codec), "eac3") || strings.EqualFold(strings.TrimSpace(codec), "ac3") {
				return false
			}
		}
	}
	return true
}
