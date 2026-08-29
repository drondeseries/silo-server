package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/plugins"
	"github.com/Silo-Server/silo-server/internal/remuxdb"
	"golang.org/x/text/language"
)

const virtualPlaybackPrefix = "virtual://"

const maxVirtualPlaybackStreams = 50

const (
	defaultMaxVirtualFailoverAttempts = 5
	virtualStartupBudget              = 60 * time.Second
	virtualProbeBudget                = 15 * time.Second
	maxVirtualPlaybackPrefetchFiles   = 2
	virtualPlaybackPrefetchBudget     = 20 * time.Second
)

func (h *PlaybackHandler) PrefetchVirtualPlayback(ctx context.Context, files []*models.MediaFile, profileID string) {
	if h == nil || h.VirtualPlaybackResolver == nil || len(files) == 0 || profileID == "" {
		return
	}
	userID := apimw.GetUserID(ctx)
	if userID == 0 {
		return
	}
	if len(files) > maxVirtualPlaybackPrefetchFiles {
		files = files[:maxVirtualPlaybackPrefetchFiles]
	}
	prefetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), virtualPlaybackPrefetchBudget)
	go func() {
		defer cancel()
		for _, file := range files {
			if prefetchCtx.Err() != nil || file == nil || !isVirtualPlaybackFile(file) {
				continue
			}
			_, _ = h.VirtualPlaybackResolver.ResolveVirtualPlayback(prefetchCtx, virtualPlaybackNeutralKey(file.FilePath), userID, profileID, file.VirtualOwnerInstallationID)
		}
	}()
}

func (h *PlaybackHandler) maxVirtualFailoverAttempts(ctx context.Context) int {
	if h != nil && h.SettingsRepo != nil {
		if raw, err := h.SettingsRepo.Get(ctx, "playback.max_virtual_failover_attempts"); err == nil && strings.TrimSpace(raw) != "" {
			if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && v > 0 {
				return v
			}
		}
	}
	if h != nil && h.PlaybackConfig != nil {
		if v := h.playbackConfig().MaxVirtualFailoverAttempts; v > 0 {
			return v
		}
	}
	return defaultMaxVirtualFailoverAttempts
}

const (
	defaultBestResultCacheTTL     = 30 * time.Minute
	defaultBestResultCacheEntries = 512
)

// VirtualBestResultCache remembers which result= URI worked for a content+profile
// pair. On replay it skips the list+resolve+probe path entirely, jumping
// directly to the known-good provider-neutral URI.
type VirtualBestResultCache struct {
	mu         sync.RWMutex
	entries    map[string]bestResultCacheEntry
	ttl        time.Duration
	maxEntries int
}

type bestResultCacheEntry struct {
	// streams holds the filtered, device-neutral candidate list. Ranking
	// happens per device on every hit (the raw provider list is identical for
	// every device, so a device-specific key would fragment cache hits).
	streams   []VirtualPlaybackStream
	expiresAt time.Time
}

// NewVirtualBestResultCache returns an initialized cache. Zero or negative ttl
// and maxEntries pick safe defaults.
func NewVirtualBestResultCache(ttl time.Duration, maxEntries int) *VirtualBestResultCache {
	if ttl <= 0 {
		ttl = defaultBestResultCacheTTL
	}
	if maxEntries <= 0 {
		maxEntries = defaultBestResultCacheEntries
	}
	return &VirtualBestResultCache{
		entries:    make(map[string]bestResultCacheEntry),
		ttl:        ttl,
		maxEntries: maxEntries,
	}
}

func (c *VirtualBestResultCache) get(key string, now time.Time) []VirtualPlaybackStream {
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok || now.After(entry.expiresAt) {
		return nil
	}
	return append([]VirtualPlaybackStream(nil), entry.streams...)
}

// Clear drops every cached result. Called on plugin lifecycle changes when
// provider configurations may have changed and cached result= URIs are
// likely stale.
func (c *VirtualBestResultCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	clear(c.entries)
	c.mu.Unlock()
}

func (c *VirtualBestResultCache) RemoveCandidate(key string, candURI string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return
	}
	candNeutral := virtualPlaybackNeutralKey(candURI)
	filtered := make([]VirtualPlaybackStream, 0, len(entry.streams))
	for _, s := range entry.streams {
		sNeutral := virtualPlaybackNeutralKey(s.URI)
		if s.URI == candURI || (candNeutral != "" && (s.URI == candNeutral || sNeutral == candURI || sNeutral == candNeutral)) {
			continue
		}
		filtered = append(filtered, s)
	}
	if len(filtered) == 0 {
		delete(c.entries, key)
		return
	}
	entry.streams = filtered
	c.entries[key] = entry
}

func (c *VirtualBestResultCache) set(key string, streams []VirtualPlaybackStream, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, k)
		}
	}
	for len(c.entries) >= c.maxEntries {
		var oldestKey string
		var oldest time.Time
		for k, entry := range c.entries {
			if oldestKey == "" || entry.expiresAt.Before(oldest) {
				oldestKey, oldest = k, entry.expiresAt
			}
		}
		delete(c.entries, oldestKey)
	}
	c.entries[key] = bestResultCacheEntry{
		streams:   streams,
		expiresAt: now.Add(c.ttl),
	}
}

// bestResultCacheKey builds a deterministic key from the content_id, neutral
// URI (without result=), and owner installation ID, with an optional device fingerprint.
func bestResultCacheKey(contentID, neutralURI string, ownerInstallationID int, deviceFingerprint ...string) string {
	raw := contentID + "\x00" + neutralURI + "\x00" + strconv.Itoa(ownerInstallationID)
	if len(deviceFingerprint) > 0 && deviceFingerprint[0] != "" {
		raw += "\x00" + deviceFingerprint[0]
	}
	digest := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(digest[:16])
}

type VirtualPlaybackResolver interface {
	ResolveVirtualPlayback(ctx context.Context, virtualPath string, userID int, profileID string, ownerInstallationID int) (string, error)
}

type VirtualPlaybackResolverFunc func(context.Context, string, int, string, int) (string, error)

func (f VirtualPlaybackResolverFunc) ResolveVirtualPlayback(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) (string, error) {
	return f(ctx, path, userID, profileID, ownerInstallationID)
}

// VirtualPlaybackStream is the provider-neutral candidate shape used by the
// just-in-time picker. Implementations must never expose provider URLs here.
type VirtualPlaybackStream struct {
	ID                  string            `json:"id"`
	Label               string            `json:"label"`
	URI                 string            `json:"uri"`
	Resolution          string            `json:"resolution,omitempty"`
	CodecVideo          string            `json:"codec_video,omitempty"`
	CodecAudio          string            `json:"codec_audio,omitempty"`
	HDR                 string            `json:"hdr,omitempty"`
	SourceType          string            `json:"source_type,omitempty"`
	FileSize            int64             `json:"file_size,omitempty"`
	Container           string            `json:"container,omitempty"`
	Bitrate             int               `json:"bitrate,omitempty"`
	FrameRate           string            `json:"frame_rate,omitempty"`
	AudioLanguages      []string          `json:"audio_languages,omitempty"`
	SubtitleLanguages   []string          `json:"subtitle_languages,omitempty"`
	HasAtmos            bool              `json:"has_atmos,omitempty"`
	QualityScore        int               `json:"quality_score,omitempty"`
	RequestHeaders      map[string]string `json:"-"`
	OwnerInstallationID int               `json:"-"`
	Visible             bool              `json:"-"`
	VisibilitySpecified bool              `json:"-"`
}

// Get* accessors satisfy plugins.VirtualStreamMetadata so the shared device
// ranker can score both this type and the plugin-layer candidate shape.
func (s VirtualPlaybackStream) GetCodecVideo() string { return s.CodecVideo }
func (s VirtualPlaybackStream) GetCodecAudio() string { return s.CodecAudio }
func (s VirtualPlaybackStream) GetHDR() string        { return s.HDR }
func (s VirtualPlaybackStream) GetContainer() string  { return s.Container }
func (s VirtualPlaybackStream) GetResolution() string { return s.Resolution }

type VirtualPlaybackStreamLister interface {
	ListVirtualPlaybackStreams(ctx context.Context, virtualPath string, userID int, profileID string, ownerInstallationID int) ([]VirtualPlaybackStream, error)
}

type VirtualPlaybackStreamListerFunc func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error)

func (f VirtualPlaybackStreamListerFunc) ListVirtualPlaybackStreams(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) ([]VirtualPlaybackStream, error) {
	return f(ctx, path, userID, profileID, ownerInstallationID)
}

// VirtualPlaybackStreamSink persists JIT candidates as selectable virtual
// files.
type VirtualPlaybackStreamSink func(context.Context, *models.MediaFile, []VirtualPlaybackStream) error

type resolvedVirtualPlaybackSource struct {
	URL            string
	URI            string
	OwnerID        int
	File           *models.MediaFile
	ProbeSucceeded bool
}

// resolveVirtualPlaybackSource chooses a ranked provider-neutral result,
// resolves it, and probes it before planning. A result URI is bound to the
// session so later Range, seek, subtitle, and transcode requests cannot silently
// switch to a technically different candidate under the original plan.
// resolveVirtualPlaybackSource resolves the provider URL for a virtual file
// and, when evidence is incomplete, probes it. deferProbe allows the start
// path to skip the synchronous probe and respond immediately on the
// deferred-metadata HLS route (see playback.DeferVirtualPlaybackMetadataV3);
// the resolved source is then probed in the background so the next play has
// complete evidence. Restart/track-change paths pass false because they need
// probed track inventory to remap selections.
func (h *PlaybackHandler) resolveVirtualPlaybackSource(r *http.Request, file *models.MediaFile, profileID string, deferProbe bool) (resolvedVirtualPlaybackSource, error) {
	if !isVirtualPlaybackFile(file) {
		return resolvedVirtualPlaybackSource{File: file}, nil
	}
	if h.VirtualPlaybackResolver == nil {
		return resolvedVirtualPlaybackSource{}, errors.New("virtual playback resolver is not configured")
	}
	userID := apimw.GetUserID(r.Context())
	parsed, _ := url.Parse(file.FilePath)
	candidates := []VirtualPlaybackStream{{
		URI: file.FilePath, OwnerInstallationID: file.VirtualOwnerInstallationID,
		Resolution: file.Resolution, CodecVideo: file.CodecVideo, CodecAudio: file.CodecAudio,
		HDR: mediaFileHDRString(file),
	}}
	noResult := parsed != nil && strings.TrimSpace(parsed.Query().Get("result")) == ""
	needsCandidateMetadata := !completeVirtualVideoEvidenceV3(file) || !completeVirtualAudioEvidenceV3(file) || !completeVirtualContainerEvidenceV3(file)
	deviceCaps, _ := h.requestDeviceCapabilities(r)
	stickyKey := bestResultCacheKey(file.ContentID, virtualPlaybackNeutralKey(file.FilePath), file.VirtualOwnerInstallationID, deviceCaps.Fingerprint())
	pinnedURI := h.peekVirtualSticky(stickyKey)
	// Check the best-result cache before listing candidates. A previous
	// successful play of this content may have a cached result= URI that
	// lets us skip the entire list+resolve+probe sequence on replay.
	if noResult && h.BestResultCache != nil {
		neutralURI := virtualPlaybackNeutralKey(file.FilePath)
		cacheKey := bestResultCacheKey(file.ContentID, neutralURI, file.VirtualOwnerInstallationID)
		if cached := h.BestResultCache.get(cacheKey, time.Now()); len(cached) > 0 {
			// Cache holds the filtered, device-neutral candidate list; rank it
			// for this device so a TV and a phone pick their own best stream
			// without another provider round-trip.
			candidates, _ = h.rankVirtualCandidatesForDevice(r, cached)
			noResult = false // treated as if file already had a result=
		}
	}
	if (noResult || needsCandidateMetadata) && h.VirtualPlaybackStreamLister != nil {
		// Candidate listing is part of the startup critical path. Keep it
		// bounded so the first-byte SLA cannot be defeated before resolution.
		listCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
		streams, err := h.VirtualPlaybackStreamLister.ListVirtualPlaybackStreams(
			listCtx, file.FilePath, userID, profileID, file.VirtualOwnerInstallationID,
		)
		if err == nil && len(streams) > 0 {
			if len(streams) > maxVirtualPlaybackStreams {
				streams = streams[:maxVirtualPlaybackStreams]
			}
			// A selected result= URI is still an active catalog row referenced by
			// the playback attempt. Refresh metadata in memory, but do not replace
			// the candidate set while this request is using that row.
			if noResult && h.VirtualPlaybackStreamSink != nil {
				visible := visibleVirtualPlaybackStreams(streams)
				_ = h.VirtualPlaybackStreamSink(listCtx, file, visible)
			}
			if filtered := filterVirtualPlaybackStreams(file, streams); len(filtered) > 0 {
				if h.BestResultCache != nil {
					neutralURI := virtualPlaybackNeutralKey(file.FilePath)
					h.BestResultCache.set(bestResultCacheKey(file.ContentID, neutralURI, file.VirtualOwnerInstallationID), filtered, time.Now())
				}
				candidates, _ = h.rankVirtualCandidatesForDevice(r, filtered)
			}
		}
		cancel()
	}
	maxAttempts := h.maxVirtualFailoverAttempts(r.Context())
	candidates = h.applyVirtualStickyPin(stickyKey, pinnedURI, candidates, deviceCaps)
	if len(candidates) > maxAttempts {
		candidates = candidates[:maxAttempts]
	}
	attemptCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), virtualStartupBudget)
	defer cancel()

	resolveAndProbe := func(i int, cand VirtualPlaybackStream) (*resolvedVirtualPlaybackSource, error) {
		oid := cand.OwnerInstallationID
		if oid <= 0 {
			oid = file.VirtualOwnerInstallationID
		}
		streamURL, resolveErr := h.VirtualPlaybackResolver.ResolveVirtualPlayback(
			attemptCtx, cand.URI, userID, profileID, oid,
		)
		if resolveErr != nil {
			return nil, resolveErr
		}
		// URL syntax and SSRF validation happen in the provider service and
		// again when the relay opens the source. Do not perform a blocking body
		// fetch here; the provider may legitimately take time before its first
		// byte, and that would consume the entire startup budget.

		transient := *file
		transient.FilePath = cand.URI
		transient.VirtualOwnerInstallationID = oid
		dbFile := (*models.MediaFile)(nil)
		if h.VirtualFileLookup != nil {
			dbFile, _ = h.VirtualFileLookup(attemptCtx, cand.URI)
		}
		if (dbFile == nil || dbFile.ID <= 0) && h.VirtualCandidateFileLookup != nil {
			dbFile, _ = h.VirtualCandidateFileLookup(attemptCtx, virtualPlaybackNeutralKey(cand.URI), file.ContentID, file.EpisodeID, oid)
		}
		if dbFile != nil && dbFile.ID > 0 {
			transient = *dbFile
			transient.FilePath = cand.URI
			transient.VirtualOwnerInstallationID = oid
		}
		if transient.Duration <= 0 {
			if file.Duration > 0 {
				transient.Duration = file.Duration
			} else if file.EpisodeID != "" && h.EpisodeLookup != nil {
				if ep, err := h.EpisodeLookup.GetByID(attemptCtx, file.EpisodeID); err == nil && ep != nil && ep.Runtime > 0 {
					transient.Duration = ep.Runtime * 60
				}
			} else if file.ContentID != "" && h.ItemLookup != nil {
				if item, err := h.ItemLookup.GetByID(attemptCtx, file.ContentID); err == nil && item != nil && item.Runtime > 0 {
					transient.Duration = item.Runtime * 60
				}
			}
		}
		hasCompleteVideoEvidence := completeVirtualVideoEvidenceV3(&transient)
		hasCompleteAudioEvidence := completeVirtualAudioEvidenceV3(&transient)
		hasCompleteContainerEvidence := completeVirtualContainerEvidenceV3(&transient)
		skipProbe := hasCompleteVideoEvidence && hasCompleteAudioEvidence && hasCompleteContainerEvidence
		if !skipProbe {
			if backfilled := h.applyVirtualStreamEvidence(attemptCtx, &transient); backfilled != nil {
				transient = *backfilled
				hasCompleteVideoEvidence = completeVirtualVideoEvidenceV3(&transient)
				hasCompleteAudioEvidence = completeVirtualAudioEvidenceV3(&transient)
				hasCompleteContainerEvidence = completeVirtualContainerEvidenceV3(&transient)
				skipProbe = hasCompleteVideoEvidence && hasCompleteAudioEvidence && hasCompleteContainerEvidence
			}
		}
		if skipProbe || h.VirtualPlaybackSourceProber == nil {
			mergeVirtualCandidateTracks(&transient, cand)
			if !transient.HDR && cand.HDR != "" {
				transient.HDR = true
			}
			h.maybeTriggerSubtitleSearch(attemptCtx, &transient, cand)
			return &resolvedVirtualPlaybackSource{
				URL: streamURL, URI: cand.URI, OwnerID: oid, File: &transient, ProbeSucceeded: skipProbe,
			}, nil
		}
		probeCtx, probeCancel := context.WithTimeout(attemptCtx, virtualProbeBudget)
		probed, probeErr := h.VirtualPlaybackSourceProber(probeCtx, streamURL, &transient)
		probeCancel()
		if probeErr != nil || probed == nil {
			slog.DebugContext(r.Context(), "virtual stream probe timed out or failed; using candidate metadata", "component", "api", "candidate_uri", cand.URI, "error", probeErr)
			mergeVirtualCandidateTracks(&transient, cand)
			if !transient.HDR && cand.HDR != "" {
				transient.HDR = true
			}
			h.maybeTriggerSubtitleSearch(attemptCtx, &transient, cand)
			return &resolvedVirtualPlaybackSource{
				URL: streamURL, URI: cand.URI, OwnerID: oid, File: &transient, ProbeSucceeded: false,
			}, nil
		}
		if transient.ID > 0 {
			probed.ID = transient.ID
			probed.MediaFolderID = transient.MediaFolderID
		}
		if transient.Duration > 0 && probed.Duration <= 0 {
			probed.Duration = transient.Duration
		}
		mergeVirtualCandidateTracks(probed, cand)
		h.maybeTriggerSubtitleSearch(probeCtx, probed, cand)
		return &resolvedVirtualPlaybackSource{
			URL: streamURL, URI: cand.URI, OwnerID: oid, File: probed, ProbeSucceeded: true,
		}, nil
	}

	var firstResolved *resolvedVirtualPlaybackSource
	var attemptErr error
	for i, candidate := range candidates {
		result, err := resolveAndProbe(i, candidate)
		if err != nil || (!result.ProbeSucceeded && candidate.URI == pinnedURI) {
			if candidate.URI == pinnedURI && h != nil {
				// The pinned source stopped working; release it so the next
				// start re-ranks candidates instead of retrying a dead URI.
				h.unpinVirtualSticky(stickyKey, candidate.URI)
			}
			if firstResolved == nil && result != nil {
				firstResolved = result
			}
			slog.WarnContext(r.Context(), "virtual playback candidate failed",
				"component", "api", "candidate_uri", candidate.URI, "candidate_index", i,
				"file_id", file.ID, "content_id", file.ContentID, "error", err)
			attemptErr = errors.Join(attemptErr, err)
			continue
		}
		if result.ProbeSucceeded || h.VirtualPlaybackSourceProber == nil {
			// Content ground truth: a probed duration wildly different from the
			// catalog runtime means the provider handed us mislabeled content.
			// Skip persisting its metadata onto this content's rows and rotate.
			expectedRuntimeMinutes := 0
			if file.EpisodeID != "" && h.EpisodeLookup != nil {
				if ep, epErr := h.EpisodeLookup.GetByID(r.Context(), file.EpisodeID); epErr == nil && ep != nil {
					expectedRuntimeMinutes = ep.Runtime
				}
			}
			if expectedRuntimeMinutes == 0 && h.ItemLookup != nil {
				if item, itemErr := h.ItemLookup.GetByID(r.Context(), file.ContentID); itemErr == nil && item != nil {
					expectedRuntimeMinutes = item.Runtime
				}
			}
			if !virtualRuntimePlausible(result.File.Duration, expectedRuntimeMinutes) {
				slog.WarnContext(r.Context(), "virtual candidate rejected: probed duration implausible",
					"component", "api", "candidate_uri", candidate.URI,
					"file_id", file.ID, "content_id", file.ContentID,
					"probed_duration_seconds", result.File.Duration,
					"expected_runtime_minutes", expectedRuntimeMinutes)
				h.unpinVirtualSticky(stickyKey, candidate.URI)
				attemptErr = errors.Join(attemptErr, fmt.Errorf("candidate %s probed duration %ds implausible for %dm runtime",
					candidate.URI, result.File.Duration, expectedRuntimeMinutes))
				continue
			}
			// Persist probed audio/subtitle tracks back to the DB so
			// the watch detail and player UI show track options on
			// subsequent views without re-probing.
			targetID := file.ID
			if result.File != nil && result.File.ID > 0 {
				targetID = result.File.ID
			}
			if h.VirtualFileMetadataSaver != nil && result.File != nil && targetID > 0 {
				videoJSON := marshalTracksJSON(sanitizeTrackSlice(result.File.VideoTracks))
				audioJSON := marshalTracksJSON(sanitizeTrackSlice(result.File.AudioTracks))
				subJSON := marshalTracksJSON(sanitizeTrackSlice(result.File.SubtitleTracks))
				if err := h.VirtualFileMetadataSaver(r.Context(), targetID, videoJSON, audioJSON, subJSON, result.File.Resolution, result.File.CodecVideo, result.File.CodecAudio, result.File.Container, result.File.HDR, result.File.Bitrate, result.File.Duration); err != nil {
					slog.ErrorContext(r.Context(), "virtual metadata persist failed", "component", "api", "file_id", targetID, "error", err)
				}
			}
			// The filtered candidate list is already cached device-neutrally
			// above (and ranked for this device), so replays skip the provider
			// round-trip and re-rank for the requesting device. Pin this URI
			// as sticky so rotation cannot churn future sessions.
			h.pinVirtualSticky(stickyKey, candidate.URI)
			return *result, nil
		}
		if firstResolved == nil {
			copy := *result
			firstResolved = &copy
		}
	}
	if firstResolved != nil {
		if len(candidates) > 0 {
			mergeVirtualCandidateTracks(firstResolved.File, candidates[0])
		}
		targetID := file.ID
		if firstResolved.File != nil && firstResolved.File.ID > 0 {
			targetID = firstResolved.File.ID
		}
		if h.VirtualFileMetadataSaver != nil && firstResolved.File != nil && targetID > 0 {
			videoJSON := marshalTracksJSON(sanitizeTrackSlice(firstResolved.File.VideoTracks))
			audioJSON := marshalTracksJSON(sanitizeTrackSlice(firstResolved.File.AudioTracks))
			subJSON := marshalTracksJSON(sanitizeTrackSlice(firstResolved.File.SubtitleTracks))
			if err := h.VirtualFileMetadataSaver(r.Context(), targetID, videoJSON, audioJSON, subJSON, firstResolved.File.Resolution, firstResolved.File.CodecVideo, firstResolved.File.CodecAudio, firstResolved.File.Container, firstResolved.File.HDR, firstResolved.File.Bitrate, firstResolved.File.Duration); err != nil {
				slog.ErrorContext(r.Context(), "virtual candidate metadata persist failed", "component", "api", "file_id", targetID, "error", err)
			}
		}
		return *firstResolved, nil
	}
	if attemptErr == nil {
		attemptErr = errors.New("virtual playback provider returned no usable stream")
	}
	// When the primary resolution fails — commonly because a previously
	// persisted "result=" candidate has rotated or expired at the provider —
	// re-rank the current provider candidates provider-neutrally and retry
	// before failing. This keeps one stale indexer/debrid result from turning
	// a still-streamable item into a hard playback failure, without crossing
	// the user's selected quality when a same-profile candidate exists.
	if fb := h.fallbackResolveStaleVirtualSource(attemptCtx, file, userID, profileID); fb != nil {
		return *fb, nil
	}
	return resolvedVirtualPlaybackSource{}, attemptErr
}

// applyVirtualStreamEvidence backfills the content-keyed aggregate evidence
// (SiloDB or crowdsourced RemuxDB) into a file whose own probe evidence is
// missing, so a cold start can choose the stream-copy remux route without a
// provider probe. Only facts the file does not already carry are filled;
// per-stream probe detail always wins.
func (h *PlaybackHandler) applyVirtualStreamEvidence(ctx context.Context, file *models.MediaFile) *models.MediaFile {
	if file == nil {
		return file
	}
	// 1. Try local SiloDB first
	if h.VirtualStreamMetadataStore != nil && file.ContentID != "" {
		cid := file.ContentID
		if file.SeasonNumber > 0 || file.EpisodeNumber > 0 {
			cid = fmt.Sprintf("%s-s%de%d", file.ContentID, file.SeasonNumber, file.EpisodeNumber)
		}
		ev, ok, err := h.VirtualStreamMetadataStore.Get(ctx, cid)
		if (err != nil || !ok || ev.Empty()) && cid != file.ContentID {
			ev, ok, err = h.VirtualStreamMetadataStore.Get(ctx, file.ContentID)
		}
		if err == nil && ok && !ev.Empty() {
			backfilled := *file
			if backfilled.Container == "" || backfilled.Container == "virtual" {
				backfilled.Container = ev.Container
			}
			if backfilled.CodecVideo == "" {
				backfilled.CodecVideo = ev.CodecVideo
			}
			if backfilled.CodecAudio == "" {
				backfilled.CodecAudio = ev.CodecAudio
			}
			if !backfilled.HDR && rangeIndicatesHDRV3(ev.VideoRange) {
				backfilled.HDR = true
			}
			if backfilled.Resolution == "" {
				backfilled.Resolution = ev.Resolution
			}
			return &backfilled
		}
	}

	// 2. Try RemuxDB crowdsourced metadata if file lacks detailed video tracks
	if (h.RemuxDBEnabled || h.RemuxDBClient != nil) && len(file.VideoTracks) == 0 {
		imdbID := remuxdb.ExtractIMDbID(file.ContentID)
		if imdbID == "" {
			imdbID = remuxdb.ExtractIMDbID(file.FilePath)
		}
		if imdbID != "" {
			client := h.RemuxDBClient
			if client == nil {
				client = remuxdb.NewClient("", "")
			}
			var seasonPtr, epPtr *int
			if file.SeasonNumber > 0 {
				s := file.SeasonNumber
				seasonPtr = &s
			}
			if file.EpisodeNumber > 0 {
				e := file.EpisodeNumber
				epPtr = &e
			}
			remuxCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			infos, err := client.FetchProbe(remuxCtx, imdbID, seasonPtr, epPtr)
			cancel()
			if err == nil && len(infos) > 0 {
				backfilled := *file
				if remuxdb.PopulateMediaFileFromRemuxDB(infos[0], &backfilled) {
					slog.InfoContext(ctx, "remuxdb evidence applied", "component", "api", "imdb_id", imdbID, "codec", backfilled.CodecVideo, "res", backfilled.Resolution)
					return &backfilled
				}
			}
		}
	}

	return file
}

// videoRangeFromV3 normalizes the probe's video range fact for the aggregate.
//
//nolint:unused // Retained for compatibility with dormant integration paths.
func videoRangeFromV3(file *models.MediaFile) string {
	if file != nil && len(file.VideoTracks) > 0 {
		if r := strings.ToLower(strings.TrimSpace(file.VideoTracks[0].VideoRange)); r != "" {
			return r
		}
	}
	if file != nil && file.HDR {
		return "hdr_unknown"
	}
	return "sdr"
}

// rangeIndicatesHDRV3 reports whether an aggregate range flag implies HDR.
func rangeIndicatesHDRV3(r string) bool {
	switch strings.ToLower(strings.TrimSpace(r)) {
	case "hdr", "hdr10", "hdr10_plus", "hlg", "dolby_vision":
		return true
	}
	return false
}

// fallbackResolveStaleVirtualSource re-lists the provider's current candidates
// and resolves the first healthy provider-neutral stream. It returns nil when
// the original URI carried no stale result= pick, or when no substitute
// candidate can be resolved, so the caller preserves its original error.
func (h *PlaybackHandler) fallbackResolveStaleVirtualSource(
	ctx context.Context,
	file *models.MediaFile,
	userID int,
	profileID string,
) *resolvedVirtualPlaybackSource {
	parsed, _ := url.Parse(file.FilePath)
	if parsed != nil && strings.TrimSpace(parsed.Query().Get("result")) == "" {
		return nil
	}
	if h.VirtualPlaybackStreamLister == nil {
		return nil
	}
	neutralKey := virtualPlaybackNeutralKey(file.FilePath)
	listCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	streams, listErr := h.VirtualPlaybackStreamLister.ListVirtualPlaybackStreams(
		listCtx, neutralKey, userID, profileID, file.VirtualOwnerInstallationID,
	)
	cancel()
	if listErr != nil {
		slog.ErrorContext(ctx, "virtual stale fallback: list failed", "component", "api", "neutral_key", neutralKey, "error", listErr)
		return nil
	}
	if len(streams) == 0 {
		slog.ErrorContext(ctx, "virtual stale fallback: no streams listed", "component", "api", "neutral_key", neutralKey)
		return nil
	}
	if len(streams) > maxVirtualPlaybackStreams {
		streams = streams[:maxVirtualPlaybackStreams]
	}
	// Guard against cross-identity candidates: only consider streams that
	// share the same scheme, host, path, and profile as the original file.
	streams = filterVirtualPlaybackStreams(file, streams)
	maxAttempts := h.maxVirtualFailoverAttempts(ctx)
	attempts := 0
	for _, stream := range streams {
		if stream.URI == "" || stream.URI == file.FilePath {
			continue
		}
		attempts++
		if attempts > maxAttempts {
			break
		}
		resolved, err := h.resolveVirtualCandidateSource(ctx, file, stream, userID, profileID)
		if err == nil {
			slog.InfoContext(ctx, "virtual stale fallback: resolved substitute", "component", "api", "original", file.FilePath, "substitute", stream.URI)
			// Persist the new working result= back to the media file so the next
			// play does not repeat the stale-fallback dance.
			if h.VirtualFileUpdater != nil && stream.URI != file.FilePath {
				if updateErr := h.VirtualFileUpdater(ctx, file.ID, stream.URI); updateErr != nil {
					slog.ErrorContext(ctx, "virtual stale fallback: persist update failed", "component", "api", "file_id", file.ID, "new_path", stream.URI, "error", updateErr)
				}
			}
			return resolved
		}
		slog.ErrorContext(ctx, "virtual stale fallback: candidate failed", "component", "api", "candidate", stream.URI, "error", err)
	}
	return nil
}

// resolveVirtualCandidateSource resolves and probes a single virtual stream
// candidate, returning a fully-probed source on success.
func (h *PlaybackHandler) resolveVirtualCandidateSource(
	ctx context.Context,
	file *models.MediaFile,
	candidate VirtualPlaybackStream,
	userID int,
	profileID string,
) (*resolvedVirtualPlaybackSource, error) {
	ownerID := candidate.OwnerInstallationID
	if ownerID <= 0 {
		ownerID = file.VirtualOwnerInstallationID
	}
	streamURL, err := h.VirtualPlaybackResolver.ResolveVirtualPlayback(
		ctx, candidate.URI, userID, profileID, ownerID,
	)
	if err != nil {
		return nil, err
	}
	transient := *file
	transient.FilePath = candidate.URI
	transient.VirtualOwnerInstallationID = ownerID
	resolved := resolvedVirtualPlaybackSource{URL: streamURL, URI: candidate.URI, OwnerID: ownerID, File: &transient}
	if h.VirtualPlaybackSourceProber == nil {
		return &resolved, nil
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, virtualProbeBudget)
	probed, probeErr := h.VirtualPlaybackSourceProber(probeCtx, streamURL, &transient)
	probeCancel()
	if probeErr != nil || probed == nil {
		return nil, errors.New("virtual stream probe failed during fallback")
	}
	resolved.File = probed
	mergeVirtualCandidateTracks(resolved.File, candidate)
	resolved.ProbeSucceeded = true
	return &resolved, nil
}

func filterVirtualPlaybackStreams(file *models.MediaFile, streams []VirtualPlaybackStream) []VirtualPlaybackStream {
	if file == nil {
		return nil
	}
	base, err := url.Parse(file.FilePath)
	if err != nil {
		return nil
	}
	baseProfile := strings.TrimSpace(base.Query().Get("profile"))
	seen := map[string]struct{}{file.FilePath: {}}
	alternatives := make([]VirtualPlaybackStream, 0, len(streams))
	for _, stream := range streams {
		if len(alternatives) >= maxVirtualPlaybackStreams-1 {
			break
		}
		stream.URI = strings.TrimSpace(stream.URI)
		if !strings.HasPrefix(strings.ToLower(stream.URI), virtualPlaybackPrefix) {
			continue
		}
		candidate, parseErr := url.Parse(stream.URI)
		if parseErr != nil ||
			!strings.EqualFold(candidate.Scheme, base.Scheme) ||
			!strings.EqualFold(candidate.Host, base.Host) ||
			candidate.EscapedPath() != base.EscapedPath() {
			continue
		}
		if baseProfile != "" && !strings.EqualFold(
			strings.TrimSpace(candidate.Query().Get("profile")), baseProfile,
		) {
			continue
		}
		if _, duplicate := seen[stream.URI]; duplicate {
			continue
		}
		seen[stream.URI] = struct{}{}
		alternatives = append(alternatives, stream)
	}
	return alternatives
}

func visibleVirtualPlaybackStreams(streams []VirtualPlaybackStream) []VirtualPlaybackStream {
	visible := make([]VirtualPlaybackStream, 0, len(streams))
	hasHidden := hasHiddenVirtualPlaybackStreams(streams)
	for _, stream := range streams {
		if stream.Visible || !hasHidden {
			visible = append(visible, stream)
		}
	}
	return visible
}

func hasHiddenVirtualPlaybackStreams(streams []VirtualPlaybackStream) bool {
	for _, stream := range streams {
		if stream.VisibilitySpecified && !stream.Visible {
			return true
		}
	}
	return false
}

func isVirtualPlaybackFile(file *models.MediaFile) bool {
	return file != nil && strings.HasPrefix(file.FilePath, virtualPlaybackPrefix)
}

func isUnplayableVirtualURI(uri string) bool {
	raw := strings.TrimSpace(strings.ToLower(uri))
	if strings.HasPrefix(raw, "virtual://series/") || strings.HasPrefix(raw, "virtual://show/") {
		parsed, err := url.Parse(raw)
		if err == nil {
			trimmed := strings.Trim(parsed.Path, "/")
			if trimmed == "" {
				return true
			}
			parts := strings.Split(trimmed, "/")
			if len(parts) < 3 {
				return true
			}
		}
	}
	return false
}

// virtualPlaybackNeutralKey returns the virtual URI with any concrete "result="
// pick removed, preserving the scheme/host/path and the profile so a stale
// provider candidate can be re-resolved provider-neutrally within the same
// quality selection.
func virtualPlaybackNeutralKey(virtualPath string) string {
	parsed, err := url.Parse(virtualPath)
	if err != nil {
		return virtualPath
	}
	q := parsed.Query()
	if strings.TrimSpace(q.Get("result")) == "" {
		return virtualPath
	}
	q.Del("result")
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

// maybeTriggerSubtitleSearch kicks off a background subtitle search when a
// virtual stream enters playback with no embedded or external subtitle tracks.
// Results are downloaded and associated with the file so they appear in the
// player's subtitle selector without blocking playback start.
func (h *PlaybackHandler) maybeTriggerSubtitleSearch(
	ctx context.Context,
	file *models.MediaFile,
	cand VirtualPlaybackStream,
) {
	if h.VirtualSubtitleSearcher == nil || file == nil {
		return
	}
	if len(file.SubtitleTracks) > 0 || len(file.ExternalSubtitles) > 0 {
		return
	}
	searchKey := any(file.ID)
	if file.ID <= 0 {
		searchKey = "virtual:" + file.ContentID + ":" + cand.URI
	}
	// Dedupe: one in-flight search per file. Rapid replays or multiple
	// candidates resolving the same file must not hammer subtitle providers.
	if _, loaded := h.SubtitleSearchInFlight.LoadOrStore(searchKey, struct{}{}); loaded {
		return
	}
	go func() {
		defer h.SubtitleSearchInFlight.Delete(searchKey)
		h.VirtualSubtitleSearcher(
			context.Background(),
			file.ContentID,
			"", // IMDb ID resolved from contentID by the caller
			"", // title resolved by the caller
			0,  // year resolved by the caller
			0,  // season
			0,  // episode
			file.ID,
			cand.SubtitleLanguages,
		)
	}()
}

// mergeVirtualCandidateTracks supplements probed virtual file tracks with
// metadata from the provider candidate. ffprobe may not always detect
// language tags, codecs, or dimensions on remote streams (especially HLS
// and DASH), so candidate metadata fills the gaps so virtual files appear
// as close to local files as possible.
func mergeVirtualCandidateTracks(probed *models.MediaFile, candidate VirtualPlaybackStream) {
	if probed == nil {
		return
	}

	// Fill empty top-level fields that ffprobe may miss on remote streams.
	if probed.Resolution == "" {
		probed.Resolution = candidate.Resolution
	}
	if probed.CodecVideo == "" {
		probed.CodecVideo = candidate.CodecVideo
	}
	if probed.CodecAudio == "" {
		probed.CodecAudio = candidate.CodecAudio
	}
	if !probed.HDR && candidate.HDR != "" {
		probed.HDR = true
	}
	if probed.Container == "" || strings.EqualFold(probed.Container, "virtual") {
		if candidate.Container != "" && !strings.EqualFold(candidate.Container, "virtual") {
			probed.Container = candidate.Container
		} else {
			probed.Container = "mkv"
		}
	}
	if probed.FileSize == 0 {
		probed.FileSize = candidate.FileSize
	}
	if probed.Bitrate == 0 {
		probed.Bitrate = candidate.Bitrate
	}
	if probed.Bitrate == 0 && probed.FileSize > 0 && probed.Duration > 0 {
		probed.Bitrate = int((probed.FileSize * 8) / int64(probed.Duration) / 1000)
	}
	if probed.Bitrate == 0 {
		probed.Bitrate = virtualBitrateFallback(probed.Resolution)
	}

	// Infer audio channels from the codec when ffprobe didn't detect them.
	channels := inferChannelsFromCodec(probed.CodecAudio)

	// Create a basic video track when ffprobe didn't detect any.
	videoCodec := probed.CodecVideo
	if videoCodec == "" {
		videoCodec = candidate.CodecVideo
	}
	if videoCodec == "" {
		videoCodec = "h264"
	}
	if probed.CodecVideo == "" {
		probed.CodecVideo = videoCodec
	}
	isDV, dvProfile := virtualDVMetadata(candidate.HDR)
	isHDR := probed.HDR || candidate.HDR != ""
	if len(probed.VideoTracks) == 0 {
		videoRange := "SDR"
		videoRangeType := "SDR"
		if isDV {
			if dvProfile == 0 {
				dvProfile = 8
			}
			videoRange = "DolbyVision"
			videoRangeType = "DOVI"
		} else if isHDR {
			videoRange = "HDR"
			videoRangeType = "HDR10"
		}
		vt := models.VideoTrack{
			Codec:          videoCodec,
			Width:          resolutionWidth(probed.Resolution),
			Height:         resolutionHeight(probed.Resolution),
			FrameRate:      defaultVirtualFrameRate(candidate.FrameRate),
			BitDepth:       8,
			Bitrate:        probed.Bitrate,
			VideoRange:     videoRange,
			VideoRangeType: videoRangeType,
			DVProfile:      dvProfile,
			DolbyVision:    virtualDVLabel(isDV, dvProfile),
		}
		if isDV && dvProfile != 5 {
			vt.DVConfigPresent = true
			vt.DVBLCompatIDPresent = true
			vt.DVBLCompatID = 1
			vt.DVBLPresent = true
		}
		probed.VideoTracks = append(probed.VideoTracks, vt)
	}
	for i := range probed.VideoTracks {
		if probed.VideoTracks[i].Width <= 0 {
			probed.VideoTracks[i].Width = resolutionWidth(probed.Resolution)
		}
		if probed.VideoTracks[i].Height <= 0 {
			probed.VideoTracks[i].Height = resolutionHeight(probed.Resolution)
		}
		if probed.VideoTracks[i].FrameRate == "" {
			probed.VideoTracks[i].FrameRate = defaultVirtualFrameRate(candidate.FrameRate)
		}
		if probed.VideoTracks[i].BitDepth <= 0 {
			probed.VideoTracks[i].BitDepth = 8
		}
		if probed.VideoTracks[i].Bitrate <= 0 {
			probed.VideoTracks[i].Bitrate = probed.Bitrate
		}
		if isDV {
			profile := probed.VideoTracks[i].DVProfile
			if profile == 0 {
				profile = dvProfile
				if profile == 0 {
					profile = 8
				}
				probed.VideoTracks[i].DVProfile = profile
			}
			if probed.VideoTracks[i].DolbyVision == "" {
				probed.VideoTracks[i].DolbyVision = virtualDVLabel(true, profile)
			}
			if probed.VideoTracks[i].VideoRange == "" || probed.VideoTracks[i].VideoRange == "SDR" {
				probed.VideoTracks[i].VideoRange = "DolbyVision"
				probed.VideoTracks[i].VideoRangeType = "DOVI"
			}
			if profile != 5 && !probed.VideoTracks[i].DVConfigPresent {
				probed.VideoTracks[i].DVConfigPresent = true
				probed.VideoTracks[i].DVBLCompatIDPresent = true
				probed.VideoTracks[i].DVBLCompatID = 1
				probed.VideoTracks[i].DVBLPresent = true
			}
		} else if isHDR && (probed.VideoTracks[i].VideoRange == "" ||
			strings.EqualFold(probed.VideoTracks[i].VideoRange, "sdr") ||
			strings.EqualFold(probed.VideoTracks[i].VideoRangeType, "sdr")) {
			probed.VideoTracks[i].VideoRange = "HDR"
			probed.VideoTracks[i].VideoRangeType = "HDR10"
		}
		if probed.VideoTracks[i].VideoRange == "" && !probed.HDR {
			probed.VideoTracks[i].VideoRange = "SDR"
			probed.VideoTracks[i].VideoRangeType = "SDR"
		}
	}

	// Synthesize audio/subtitle tracks from the provider-declared languages
	// when the probe left the inventory empty. ffprobe may not always detect
	// language tags on remote streams (especially HLS and DASH), so candidate
	// languages fill the gap so the player can display track choices before
	// and during playback. Existing probed tracks are never overwritten.
	mergeVirtualCandidateLanguages(probed, candidate)

	// Fill audio channels and codec on existing tracks that lack them.
	for i := range probed.AudioTracks {
		if probed.AudioTracks[i].Codec == "" && probed.CodecAudio != "" {
			probed.AudioTracks[i].Codec = probed.CodecAudio
		}
		if probed.AudioTracks[i].Channels == 0 {
			probed.AudioTracks[i].Channels = channels
		}
	}

	if len(probed.AudioTracks) > 0 {
		hasDefault := false
		for _, t := range probed.AudioTracks {
			if t.Default {
				hasDefault = true
				break
			}
		}
		if !hasDefault {
			probed.AudioTracks[0].Default = true
		}
	}

}

var virtualDVProfileRegex = regexp.MustCompile(`(?i)(?:profile|dovi|dv|dolby\s*vision)\s*[-._:]?\s*0*([1-9]|1\d|20)(?:[.]\d+)?(?:[^a-z0-9]|$)`)

func virtualDVMetadata(raw string) (bool, int) {
	lower := strings.ToLower(strings.TrimSpace(raw))
	isDV := strings.Contains(lower, "dolby vision") || strings.Contains(lower, "dovi") ||
		lower == "dv" || strings.HasPrefix(lower, "dv ") || strings.HasSuffix(lower, " dv") || strings.Contains(lower, " dv ") ||
		virtualDVProfileMarker(lower)
	if !isDV {
		return false, 0
	}
	if matches := virtualDVProfileRegex.FindStringSubmatch(lower); len(matches) > 1 {
		if profile, err := strconv.Atoi(matches[1]); err == nil && profile > 0 && profile <= 20 {
			return true, profile
		}
	}
	return true, 0
}

func virtualDVProfileMarker(raw string) bool {
	for _, profile := range []string{"profile 5", "profile 7", "profile 8", "dv5", "dv7", "dv8"} {
		if strings.Contains(raw, profile) {
			return true
		}
	}
	return false
}

func mediaFileHDRString(file *models.MediaFile) string {
	if file == nil {
		return ""
	}
	if dvProfile := file.PrimaryDVProfile(); dvProfile > 0 {
		return fmt.Sprintf("Dolby Vision Profile %d", dvProfile)
	}
	if len(file.VideoTracks) > 0 {
		vt := file.VideoTracks[0]
		if vt.DolbyVision != "" {
			return vt.DolbyVision
		}
		if strings.EqualFold(vt.VideoRange, "DolbyVision") || strings.EqualFold(vt.VideoRangeType, "DOVI") {
			return "Dolby Vision"
		}
		if vt.VideoRange != "" && !strings.EqualFold(vt.VideoRange, "SDR") {
			return vt.VideoRange
		}
	}
	if file.HDR {
		return "true"
	}
	return ""
}

func virtualDVLabel(isDV bool, profile int) string {
	if !isDV || profile <= 0 {
		return ""
	}
	return "Profile " + strconv.Itoa(profile)
}

// mergeVirtualCandidateLanguages appends provider-declared audio languages as
// tracks when the probed inventory does not already carry them. Candidate
// language lists come from the release metadata (e.g. ITA-ENG in a release
// name), not from ffprobe, so tracks synthesized here are never authoritative —
// a later probe fills real codec/channel evidence on top. Release-group markers
// that are not real languages (e.g. MULTI, DUAL) are skipped so a bogus track
// never appears in the player's picker.
//
// Provider-declared subtitle languages are deliberately NOT synthesized into
// embedded tracks here: a synthesized SubtitleTrack carries a stream ordinal
// that no real subtitle stream backs, and the extractor maps it straight to
// ffmpeg's `0:s:N` specifier. That produced phantom subtitle selections that
// always failed at FFmpeg ("Stream map ” matches no streams"). Real embedded
// subtitle inventory comes only from the probe; provider subtitle hints stay on
// the candidate stream for the picker and drive the background subtitle search.
func mergeVirtualCandidateLanguages(probed *models.MediaFile, candidate VirtualPlaybackStream) {
	if probed == nil {
		return
	}
	audioCodec := probed.CodecAudio
	if audioCodec == "" {
		audioCodec = candidate.CodecAudio
	}
	if audioCodec == "" {
		audioCodec = "aac"
	}
	channels := inferChannelsFromCodec(audioCodec)
	if len(candidate.AudioLanguages) > 0 {
		existing := make(map[string]bool, len(probed.AudioTracks))
		for _, t := range probed.AudioTracks {
			if lang := strings.TrimSpace(t.Language); lang != "" {
				existing[strings.ToLower(lang)] = true
			}
		}
		for _, lang := range candidate.AudioLanguages {
			lang = strings.TrimSpace(lang)
			if lang == "" || !isRealVirtualLanguageTag(lang) || existing[strings.ToLower(lang)] {
				continue
			}
			existing[strings.ToLower(lang)] = true
			probed.AudioTracks = append(probed.AudioTracks, models.AudioTrack{
				Language: lang,
				Codec:    audioCodec,
				Channels: channels,
			})
		}
	}
}

// isRealVirtualLanguageTag reports whether a provider-declared language token
// parses as a real ISO language subtag. The naive lowercased canonical form
// cannot tell "multi" from a valid three-letter code like "fil", so parsing
// with the Unicode language tagger and requiring a concrete base subtag keeps
// release markers like MULTI/DUAL out of the synthesized track inventory.
func isRealVirtualLanguageTag(value string) bool {
	tag, err := language.Parse(value)
	if err != nil {
		return false
	}
	base, conf := tag.Base()
	return conf != language.No && base.String() != ""
}

func defaultVirtualFrameRate(rate string) string {
	if strings.TrimSpace(rate) == "" {
		return "24"
	}
	return rate
}

func virtualBitrateFallback(resolution string) int {
	switch strings.ToLower(strings.TrimSpace(resolution)) {
	case "2160p", "4k", "uhd":
		return 24000
	case "720p":
		return 5000
	case "480p":
		return 2500
	default:
		return 10000
	}
}

// inferChannelsFromCodec returns a plausible channel count for a codec string.
func inferChannelsFromCodec(codec string) int {
	switch strings.ToLower(codec) {
	case "atmos":
		return 8
	case "truehd", "dts-hd", "dts", "eac3", "ac3":
		return 6
	default:
		return 2
	}
}

// resolutionWidth returns a typical width for a resolution label.
func resolutionWidth(label string) int {
	switch strings.ToLower(label) {
	case "2160p":
		return 3840
	case "1080p":
		return 1920
	case "720p":
		return 1280
	case "480p":
		return 720
	default:
		return 0
	}
}

// resolutionHeight returns a typical height for a resolution label.
func resolutionHeight(label string) int {
	switch strings.ToLower(label) {
	case "2160p":
		return 2160
	case "1080p":
		return 1080
	case "720p":
		return 720
	case "480p":
		return 480
	default:
		return 0
	}
}

// canSkipProbeForContainer returns true for container formats that ffmpeg
// handles natively without needing ffprobe metadata. When a candidate
// already declares codecs, we skip the probe for these formats.
// ffprobe may return compound formats like "matroska,webm" or capitalized
// variants like "Matroska"; we check whether any recognized token appears.
func canSkipProbeForContainer(container string) bool {
	lowered := strings.ToLower(strings.TrimSpace(container))
	if lowered == "" {
		return false
	}
	known := []string{"mp4", "mkv", "webm", "ts", "m2ts", "mov", "avi", "flv", "wmv", "m4v", "mpeg", "mpg", "ogv", "3gp", "matroska"}
	for _, k := range known {
		if lowered == k || strings.Contains(lowered, k) {
			return true
		}
	}
	return false
}

// completeVirtualVideoEvidenceV3 reports whether a virtual file carries the
// detailed ffprobe video evidence the v3 planner needs to validate direct-play
// and stream-copy remux routes (profile/level/bit-depth/dimensions/frame rate/
// bitrate), as opposed to bare candidate codec declarations.
func completeVirtualVideoEvidenceV3(file *models.MediaFile) bool {
	if file == nil || len(file.VideoTracks) == 0 || file.VideoTracks[0].Codec == "" {
		return false
	}
	track := file.VideoTracks[0]
	if track.Width <= 0 || track.Height <= 0 || track.FrameRate == "" {
		return false
	}
	if file.CodecVideo == "" || file.Resolution == "" {
		return false
	}
	return true
}

// completeVirtualAudioEvidenceV3 reports whether a virtual file carries the
// probed audio track evidence (codecs, channels, and layouts) required for
// the planner to validate direct play and surround sound passthrough.
func completeVirtualAudioEvidenceV3(file *models.MediaFile) bool {
	if file == nil {
		return false
	}
	if file.IsAudioOnly() {
		return len(file.AudioTracks) > 0 && file.CodecAudio != ""
	}
	return len(file.AudioTracks) > 0 && file.AudioTracks[0].Codec != "" && file.AudioTracks[0].Channels > 0
}

// completeVirtualContainerEvidenceV3 reports whether the file's real container
// is known and usable for a server-mediated route. The canonical virtual row
// keeps Container="virtual" until a probe fills it in; once a prior play
// persisted the real container (mkv/webm/mp4/...), re-probing the same stream
// on every cold start adds nothing but a provider round-trip.
func completeVirtualContainerEvidenceV3(file *models.MediaFile) bool {
	if file == nil {
		return false
	}
	return canSkipProbeForContainer(file.Container)
}

// marshalTracksJSON safely marshals track slices to JSON bytes for DB storage.
func marshalTracksJSON(tracks any) []byte {
	if tracks == nil {
		return []byte("[]")
	}
	data, err := json.Marshal(tracks)
	if err != nil {
		return []byte("[]")
	}
	return data
}

// sanitizeTrackSlice ensures the value is a slice/array, not a scalar.
// PostgreSQL jsonb array operations (like in triggers) fail with
// "cannot extract elements from a scalar" when given non-array jsonb.
func sanitizeTrackSlice(v any) any {
	if v == nil {
		return []any{}
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		return v
	}
	// Wrap scalar in a slice
	return []any{v}
}

// virtualStickyPin remembers the last virtual candidate that played
// successfully for a content key.
type virtualStickyPin struct {
	uri      string
	pinnedAt time.Time
}

// virtualStickyTTL bounds how long a pin can steer selection without being
// re-confirmed by another successful play.
const (
	virtualStickyTTL     = 24 * time.Hour
	virtualStickyMaxPins = 4096
)

// virtualRuntimeTolerance is the maximum fraction a probed candidate duration
// may deviate from the catalog runtime before the candidate is treated as
// mislabeled content rather than a legitimate release.
//
// 0.30 accommodates anime and international content where fansub groups trim
// OP/ED sequences and streaming cuts differ from broadcast metadata by 3–5
// minutes on a typical 24-minute episode. Genuinely wrong content (a trailer,
// a sample, or a different show) differs by far more than this.
const virtualRuntimeTolerance = 0.30

// virtualRuntimePlausible reports whether a probed candidate's duration is
// consistent with the item's catalog runtime. Unknown values pass: the gate
// only rejects when both sides are known and clearly disagree.
func virtualRuntimePlausible(probedSeconds, runtimeMinutes int) bool {
	if probedSeconds <= 0 || runtimeMinutes <= 0 {
		return true
	}
	expected := float64(runtimeMinutes) * 60
	diff := math.Abs(float64(probedSeconds) - expected)
	return diff <= expected*virtualRuntimeTolerance
}

// peekVirtualSticky returns the pinned candidate URI for key, if fresh.
func (h *PlaybackHandler) peekVirtualSticky(key string) string {
	if h == nil || key == "" {
		return ""
	}
	h.virtualStickyMu.Lock()
	defer h.virtualStickyMu.Unlock()
	pin, ok := h.virtualStickyPins[key]
	if !ok {
		return ""
	}
	if time.Since(pin.pinnedAt) > virtualStickyTTL {
		delete(h.virtualStickyPins, key)
		return ""
	}
	return pin.uri
}

// pinVirtualSticky records uri as the sticky candidate for key. The map grows
// by one entry per distinct virtual content key; entries expire lazily on
// access, so no sweeper goroutine is needed.
func (h *PlaybackHandler) pinVirtualSticky(key, uri string) {
	if h == nil || key == "" || uri == "" {
		return
	}
	h.virtualStickyMu.Lock()
	defer h.virtualStickyMu.Unlock()
	if h.virtualStickyPins == nil {
		h.virtualStickyPins = make(map[string]virtualStickyPin)
	}
	// Opportunistic hygiene on write: drop expired pins and bound the map so
	// long-lived processes cannot accumulate one entry per content key forever.
	now := time.Now()
	for k, pin := range h.virtualStickyPins {
		if now.Sub(pin.pinnedAt) > virtualStickyTTL {
			delete(h.virtualStickyPins, k)
		}
	}
	for len(h.virtualStickyPins) >= virtualStickyMaxPins {
		oldestKey := ""
		var oldest time.Time
		for k, pin := range h.virtualStickyPins {
			if oldestKey == "" || pin.pinnedAt.Before(oldest) {
				oldestKey, oldest = k, pin.pinnedAt
			}
		}
		if oldestKey == "" {
			break
		}
		delete(h.virtualStickyPins, oldestKey)
	}
	h.virtualStickyPins[key] = virtualStickyPin{uri: uri, pinnedAt: now}
}

// unpinVirtualSticky releases the pin for key when it still refers to uri,
// so a source that stopped working cannot steer later starts.
func (h *PlaybackHandler) unpinVirtualSticky(key, uri string) {
	if h == nil || key == "" || uri == "" {
		return
	}
	h.virtualStickyMu.Lock()
	defer h.virtualStickyMu.Unlock()
	if pin, ok := h.virtualStickyPins[key]; ok && pin.uri == uri {
		delete(h.virtualStickyPins, key)
	}
}

// applyVirtualStickyPin moves the pinned candidate to the front of the list
// while it is still offered, provided it meets score parity with the device-preferred
// candidate at index 0. If the pinned candidate has vanished or has a lower compatibility
// score for the requesting device, it is not promoted.
func (h *PlaybackHandler) applyVirtualStickyPin(key, pinnedURI string, candidates []VirtualPlaybackStream, device ...plugins.DeviceCapabilities) []VirtualPlaybackStream {
	if pinnedURI == "" || len(candidates) == 0 {
		return candidates
	}
	pinnedIdx := -1
	for i, cand := range candidates {
		if cand.URI == pinnedURI {
			pinnedIdx = i
			break
		}
	}
	if pinnedIdx < 0 {
		// Pinned URI vanished from the offered set.
		h.unpinVirtualSticky(key, pinnedURI)
		return candidates
	}
	if pinnedIdx == 0 {
		return candidates
	}

	// Score parity guard: if device capabilities are present, only promote the pinned
	// candidate if its score is >= the device-ranked candidate at index 0.
	if len(device) > 0 {
		d := device[0]
		if len(d.CodecsVideo) > 0 || len(d.CodecsAudio) > 0 || d.HDR || d.DolbyVision || d.MaxResolution != "" {
			pinnedScore := plugins.ScoreCandidate(&candidates[pinnedIdx], d)
			topScore := plugins.ScoreCandidate(&candidates[0], d)
			if pinnedScore < topScore {
				return candidates
			}
		}
	}

	cand := candidates[pinnedIdx]
	reordered := make([]VirtualPlaybackStream, 0, len(candidates))
	reordered = append(reordered, cand)
	reordered = append(reordered, candidates[:pinnedIdx]...)
	reordered = append(reordered, candidates[pinnedIdx+1:]...)
	return reordered
}
