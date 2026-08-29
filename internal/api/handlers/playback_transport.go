package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/logredact"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/nodepool"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/tonemap"
	"github.com/Silo-Server/silo-server/internal/transcodenode"
)

// startLocalPlaybackTransport is the shared local ffmpeg launch primitive for
// legacy and protocol-v3 orchestration. Callers retain ownership of lifecycle
// locking and decide whether registration is immediate or transactionally
// staged.
type localPlaybackStartupError struct {
	cause   error
	running bool
}

func (e *localPlaybackStartupError) Error() string {
	return e.cause.Error()
}

func (e *localPlaybackStartupError) Unwrap() error {
	return e.cause
}

func (h *PlaybackHandler) startLocalPlaybackTransport(ctx context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
	// Tone-map hardware/software selection and retries are owned by the v3
	// planner (softwareToneMapRetryOptsV3) and the compat recipe resolver; this
	// primitive starts exactly the executor it was given.
	return h.startLocalPlaybackTransportOnce(ctx, opts)
}

func (h *PlaybackHandler) startLocalPlaybackTransportOnce(ctx context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
	if !strings.HasPrefix(strings.ToLower(opts.InputPath), virtualPlaybackPrefix) {
		session, startErr := playback.StartTranscode(context.WithoutCancel(ctx), opts)
		if startErr != nil {
			return nil, startErr
		}
		if _, readyErr := session.WaitForManifest(8 * time.Second); readyErr != nil {
			startupErr := &localPlaybackStartupError{cause: readyErr, running: session.IsRunning()}
			_ = session.Close()
			return nil, startupErr
		}
		return session, nil
	}
	// The catalog candidate can be replaced while playback is starting. Do not
	// make a just-selected virtual row a second hard dependency; the canonical
	// URI and owner carried in opts are sufficient to resolve the provider URL.
	file := &models.MediaFile{ID: opts.MediaFileID, FilePath: opts.InputPath, VirtualOwnerInstallationID: opts.VirtualSourceOwnerInstallationID}
	if h.fileResolver != nil && opts.MediaFileID > 0 {
		if catalogFile, lookupErr := h.fileResolver.GetByID(ctx, opts.MediaFileID); lookupErr == nil && catalogFile != nil {
			origPath := file.FilePath
			file = catalogFile
			if isUnplayableVirtualURI(file.FilePath) && !isUnplayableVirtualURI(origPath) {
				file.FilePath = origPath
			}
		}
	}
	userID, profileID := 0, ""
	ownerInstallationID := file.VirtualOwnerInstallationID
	sessionVirtualURI := ""
	if session, sessionErr := h.sessionMgr.GetSession(opts.SessionID); sessionErr == nil && session != nil {
		userID, profileID = session.UserID, session.ProfileID
		sessionVirtualURI = session.VirtualSourceURI
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(session.VirtualSourceURI)), virtualPlaybackPrefix) && (!isUnplayableVirtualURI(session.VirtualSourceURI) || isUnplayableVirtualURI(file.FilePath)) {
			copy := *file
			copy.FilePath = session.VirtualSourceURI
			if session.VirtualSourceOwnerInstallationID > 0 {
				copy.VirtualOwnerInstallationID = session.VirtualSourceOwnerInstallationID
			}
			file = &copy
			ownerInstallationID = file.VirtualOwnerInstallationID
			opts.InputPath = file.FilePath
		}
	}
	canonicalPath := opts.InputPath
	neutralPath := virtualPlaybackNeutralKey(file.FilePath)
	if neutralPath == "" || strings.HasSuffix(neutralPath, "://") {
		neutralPath = virtualPlaybackNeutralKey(opts.InputPath)
	}
	opts.CanonicalInputPath = canonicalPath
	opts.VirtualSourceOwnerInstallationID = ownerInstallationID
	opts.RefreshInput = func(refreshCtx context.Context) (string, func(), error) {
		// A restart renews the exact candidate pinned to this session, never a
		// provider-neural re-selection: re-resolving through the neutral path
		// can silently swap to a differently-ranked candidate mid-stream. The
		// canonical path still carries the ?result= identity the session bound
		// to during planning.
		return h.resolveVirtualInputURI(
			refreshCtx, canonicalPath, ownerInstallationID,
			userID, profileID, true,
		)
	}
	var lastErr error
	startupCtx, startupCancel := context.WithTimeout(context.WithoutCancel(ctx), virtualStartupBudget)
	defer startupCancel()
	maxAttempts := h.maxVirtualFailoverAttempts(ctx)
	if sessionVirtualURI != "" {
		maxAttempts = min(2, maxAttempts)
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		targetURI := canonicalPath
		if attempt > 0 {
			targetURI = neutralPath
		}
		relayURL, cleanup, resolveErr := h.resolveVirtualInputURI(
			startupCtx, targetURI, ownerInstallationID, userID, profileID, attempt > 0,
		)
		if resolveErr != nil {
			lastErr = resolveErr
			if h.BestResultCache != nil && file != nil {
				neutralURI := virtualPlaybackNeutralKey(canonicalPath)
				h.BestResultCache.RemoveCandidate(bestResultCacheKey(file.ContentID, neutralURI, ownerInstallationID), targetURI)
			}
			if clearer, ok := h.fileResolver.(interface {
				ClearVirtualResultPin(context.Context, int) error
			}); ok && file != nil && file.ID > 0 &&
				strings.Contains(file.FilePath, "?result=") && targetURI == canonicalPath {
				unpinCtx, unpinCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				if err := clearer.ClearVirtualResultPin(unpinCtx, file.ID); err != nil {
					slog.WarnContext(ctx, "failed to clear virtual result pin after transport failure", "component", "api", "file_id", file.ID, "error", err)
				} else {
					file.FilePath = virtualPlaybackNeutralKey(file.FilePath)
					canonicalPath = file.FilePath
				}
				unpinCancel()
			}
			continue
		}
		transcodeCtx, transcodeCancel := context.WithCancel(context.Background())
		timer := time.AfterFunc(4*time.Hour, transcodeCancel)
		cleanupWithCancel := func() {
			timer.Stop()
			transcodeCancel()
			if cleanup != nil {
				cleanup()
			}
		}
		attemptOpts := opts
		attemptOpts.InputPath = relayURL
		attemptOpts.InputCleanup = cleanupWithCancel
		session, startErr := playback.StartTranscode(transcodeCtx, attemptOpts)
		if startErr == nil {
			if _, readyErr := session.WaitForManifest(playback.ManifestStartupTimeout); readyErr == nil {
				return session, nil
			} else {
				startErr = readyErr
			}
			_ = session.Close()
		} else if cleanup != nil {
			cleanupWithCancel()
		}
		if h.BestResultCache != nil && file != nil {
			neutralURI := virtualPlaybackNeutralKey(canonicalPath)
			h.BestResultCache.RemoveCandidate(bestResultCacheKey(file.ContentID, neutralURI, ownerInstallationID), targetURI)
		}
		// A pinned ?result= row whose link just failed would start every later
		// play on the same dead candidate; un-pin it so the next attempt's
		// first resolve lists live candidates. Failover below already refreshes
		// within this attempt chain — this heals the row for the plays after it.
		if clearer, ok := h.fileResolver.(interface {
			ClearVirtualResultPin(context.Context, int) error
		}); ok && file != nil && file.ID > 0 &&
			strings.Contains(file.FilePath, "?result=") && targetURI == canonicalPath {
			unpinCtx, unpinCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			if err := clearer.ClearVirtualResultPin(unpinCtx, file.ID); err != nil {
				slog.WarnContext(ctx, "failed to clear virtual result pin after transport failure", "component", "api", "file_id", file.ID, "error", err)
			} else {
				file.FilePath = virtualPlaybackNeutralKey(file.FilePath)
				canonicalPath = file.FilePath
			}
			unpinCancel()
		}
		lastErr = startErr
	}
	if lastErr == nil {
		lastErr = errors.New("virtual transcode provider returned no usable stream")
	}
	return nil, lastErr
}

func (h *PlaybackHandler) resolveVirtualInputURI(
	ctx context.Context,
	virtualURI string,
	ownerInstallationID int,
	userID int,
	profileID string,
	forceRefresh bool,
) (string, func(), error) {
	var inputPath string
	var err error
	if forceRefresh && h.VirtualMediaRefreshResolver != nil {
		inputPath, err = h.VirtualMediaRefreshResolver.RefreshVirtualMedia(
			ctx, virtualURI, ownerInstallationID, userID, profileID,
		)
	} else {
		inputPath, err = resolveVirtualMediaPath(
			ctx, h.VirtualMediaResolver, virtualURI,
			ownerInstallationID, userID, profileID,
		)
	}
	if err != nil {
		return "", nil, fmt.Errorf("resolve virtual input: %w", err)
	}
	if h.RemoteStreamRelay == nil {
		return inputPath, nil, nil
	}
	if h.AllowInsecureVirtual != nil && h.AllowInsecureVirtual(ownerInstallationID) {
		return h.RemoteStreamRelay.RegisterInsecure(ctx, inputPath)
	}
	return h.RemoteStreamRelay.Register(ctx, inputPath)
}

// startRemotePlaybackTransport is the shared remote-node launch primitive.
// It returns the node's HTTP status separately so legacy and v3 can preserve
// their existing public error envelopes while executing identical transport
// startup and response parsing.
func (h *PlaybackHandler) startRemotePlaybackTransport(ctx context.Context, nodeURL string, request transcodenode.TranscodeStartRequest) (transcodenode.TranscodeStartResponse, int, error) {
	if strings.HasPrefix(strings.ToLower(request.InputPath), virtualPlaybackPrefix) {
		return transcodenode.TranscodeStartResponse{}, 0, errors.New("virtual sources require an integrated transcode transport")
	}
	body, err := json.Marshal(request)
	if err != nil {
		return transcodenode.TranscodeStartResponse{}, 0, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, h.remotePlaybackTransportTimeout(nodeURL, request))
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, nodepool.NodeEndpoint(nodeURL, "/transcode/start"), bytes.NewReader(body))
	if err != nil {
		return transcodenode.TranscodeStartResponse{}, 0, logredact.SanitizeURLError(err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+h.JWTSecret)
	response, err := http.DefaultClient.Do(httpRequest)
	if err != nil {
		return transcodenode.TranscodeStartResponse{}, 0, logredact.SanitizeURLError(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusAccepted {
		// Drain the (small) error body so the transport can reuse the
		// connection instead of tearing it down on every failed start.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if request.ToneMapMode != "" {
			if validationErr := transcodenode.ToneMapExecutionErrorForResponse(
				response.StatusCode,
				response.Header.Get(transcodenode.ToneMapExecutionErrorHeader),
			); validationErr != nil {
				return transcodenode.TranscodeStartResponse{}, response.StatusCode, validationErr
			}
		}
		return transcodenode.TranscodeStartResponse{}, response.StatusCode, nil
	}
	var result transcodenode.TranscodeStartResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		// Older nodes returned an empty 202 response; accept that for ordinary
		// transcodes while treating any other malformed 202 body as a failed
		// start instead of fabricating a success from a zero-value response.
		if errors.Is(err, io.EOF) && request.ToneMapMode == "" {
			return transcodenode.TranscodeStartResponse{}, response.StatusCode, nil
		}
		slog.WarnContext(ctx, "remote transcode start response decode failed", "component", "api", "node", logredact.SanitizeURL(nodeURL), "error", err)
		return transcodenode.TranscodeStartResponse{}, response.StatusCode, fmt.Errorf("decode remote transcode start response: %w", err)
	}
	return result, response.StatusCode, nil
}

func (h *PlaybackHandler) remotePlaybackTransportTimeout(nodeURL string, request transcodenode.TranscodeStartRequest) time.Duration {
	if request.ToneMapMode == "" {
		return 20 * time.Second
	}
	timeout := h.remoteToneMapProbeTimeoutV3(nodeURL) + playback.ManifestStartupTimeout
	if request.ToneMapPreflightRequired {
		timeout += tonemap.SourcePreflightTimeout(request.TotalDuration)
	}
	if request.RequireReady {
		timeout += transcodenode.TranscodeStartReadinessTimeout
	}
	return timeout
}

func fetchRemoteTranscodeCapabilities(ctx context.Context, nodeURL, jwtSecret string) (playback.HWAccelInfo, error) {
	info, status, err := transcodenode.FetchHWCapabilities(ctx, http.DefaultClient, nodeURL, jwtSecret)
	if err != nil {
		return playback.HWAccelInfo{}, err
	}
	if status != http.StatusOK {
		return playback.HWAccelInfo{}, fmt.Errorf("node returned %d", status)
	}
	info.Source = "transcode_node"
	info.NodeURL = nodeURL
	return info, nil
}
