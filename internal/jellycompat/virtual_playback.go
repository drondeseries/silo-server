package jellycompat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/httpstream"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/remotestream"
)

const (
	compatVirtualScheme           = "virtual://"
	compatVirtualListTimeout      = 8 * time.Second
	compatVirtualProbeTimeout     = 10 * time.Second
	compatVirtualStartupTimeout   = 45 * time.Second
	compatVirtualMaxProbeAttempts = 5
)

// VirtualMediaResolver resolves a provider-neutral virtual URI to a temporary
// provider URL. Implementations must keep credentials out of returned errors.
type VirtualMediaResolver interface {
	ResolveVirtualMedia(ctx context.Context, virtualURI string, ownerInstallationID, userID int, profileID string) (string, error)
}

type VirtualMediaResolverFunc func(context.Context, string, int, int, string) (string, error)

func (f VirtualMediaResolverFunc) ResolveVirtualMedia(ctx context.Context, virtualURI string, ownerInstallationID, userID int, profileID string) (string, error) {
	return f(ctx, virtualURI, ownerInstallationID, userID, profileID)
}

// VirtualMediaRefreshResolver bypasses a provider's short-lived result cache.
type VirtualMediaRefreshResolver interface {
	RefreshVirtualMedia(ctx context.Context, virtualURI string, ownerInstallationID, userID int, profileID string) (string, error)
}

type VirtualMediaRefreshResolverFunc func(context.Context, string, int, int, string) (string, error)

func (f VirtualMediaRefreshResolverFunc) RefreshVirtualMedia(ctx context.Context, virtualURI string, ownerInstallationID, userID int, profileID string) (string, error) {
	return f(ctx, virtualURI, ownerInstallationID, userID, profileID)
}

// VirtualPlaybackStream is the provider-neutral portion of a current provider
// result. Temporary provider URLs never cross this interface.
type VirtualPlaybackStream struct {
	URI                 string   `json:"uri"`
	Label               string   `json:"label"`
	Resolution          string   `json:"resolution,omitempty"`
	CodecVideo          string   `json:"codec_video,omitempty"`
	CodecAudio          string   `json:"codec_audio,omitempty"`
	HDR                 string   `json:"hdr,omitempty"`
	Container           string   `json:"container,omitempty"`
	FileSize            int64    `json:"file_size,omitempty"`
	Bitrate             int      `json:"bitrate,omitempty"`
	AudioLanguages      []string `json:"audio_languages,omitempty"`
	SubtitleLanguages   []string `json:"subtitle_languages,omitempty"`
	OwnerInstallationID int      `json:"-"`
}

type VirtualPlaybackStreamLister interface {
	ListVirtualPlaybackStreams(ctx context.Context, virtualURI string, userID int, profileID string, ownerInstallationID int) ([]VirtualPlaybackStream, error)
}

type VirtualPlaybackStreamListerFunc func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error)

func (f VirtualPlaybackStreamListerFunc) ListVirtualPlaybackStreams(ctx context.Context, virtualURI string, userID int, profileID string, ownerInstallationID int) ([]VirtualPlaybackStream, error) {
	return f(ctx, virtualURI, userID, profileID, ownerInstallationID)
}

type VirtualSourceProber func(context.Context, string, *models.MediaFile) (*models.MediaFile, error)

// RemoteStreamRelay is the credential-hiding, SSRF-protected transport shared
// by direct delivery and FFmpeg inputs.
type RemoteStreamRelay interface {
	Proxy(http.ResponseWriter, *http.Request, string) error
	ProxyInsecure(http.ResponseWriter, *http.Request, string) error
	Register(context.Context, string) (string, func(), error)
}

type insecureRemoteStreamRegistrar interface {
	RegisterInsecure(context.Context, string) (string, func(), error)
}

func registerRemoteStreamInput(ctx context.Context, relay RemoteStreamRelay, resolved string, insecure bool) (string, func(), error) {
	if insecure {
		if registrar, ok := relay.(insecureRemoteStreamRegistrar); ok {
			return registrar.RegisterInsecure(ctx, resolved)
		}
	}
	return relay.Register(ctx, resolved)
}

func isCompatVirtualPath(path string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(path)), compatVirtualScheme)
}

func isCompatVirtualFile(file *models.MediaFile) bool {
	return file != nil && (isCompatVirtualPath(file.FilePath) || strings.EqualFold(strings.TrimSpace(file.Container), "virtual"))
}

func isCompatVirtualSource(source PlaybackMediaSource) bool {
	return isCompatVirtualPath(source.VirtualSourceURI) || isCompatVirtualPath(source.Version.FilePath) || strings.EqualFold(strings.TrimSpace(source.Version.Container), "virtual")
}

// Dedicated nodes cannot resolve provider-neutral virtual identities or use
// the central server's relay, so virtual playback stays on the integrated path.
func shouldUseCompatNodePool(source PlaybackMediaSource, file *models.MediaFile) bool {
	return !isCompatVirtualSource(source) && !isCompatVirtualFile(file)
}

// boundVirtualDownloadSource returns the exact provider-neutral source chosen
// during PlaybackInfo. A file-ID fallback covers clients that omit or rewrite
// MediaSourceId on their download/range requests without permitting a source
// from another file to be substituted.
func (h *PlaybackHandler) boundVirtualDownloadSource(session *Session, playSessionID string, fileID int, mediaSourceID string) *PlaybackMediaSource {
	if h == nil || h.playbackStore == nil || session == nil || strings.TrimSpace(playSessionID) == "" {
		return nil
	}
	playSession, ok := h.playbackStore.Get(playSessionID)
	if !ok || playSession.CompatToken != session.Token {
		return nil
	}
	if source := findMediaSource(playSession, mediaSourceID); source != nil && source.FileID == fileID && isCompatVirtualSource(*source) {
		return source
	}
	for i := range playSession.MediaSources {
		source := playSession.MediaSources[i]
		if source.FileID == fileID && isCompatVirtualSource(source) {
			return &source
		}
	}
	return nil
}

type resolvedCompatVirtualSource struct {
	file    *models.MediaFile
	uri     string
	ownerID int
}

func (h *PlaybackHandler) prepareVirtualPlaybackVersion(ctx context.Context, session *Session, version catalog.FileVersion) (catalog.FileVersion, string, int, error) {
	if !isCompatVirtualPath(version.FilePath) && !strings.EqualFold(strings.TrimSpace(version.Container), "virtual") {
		return version, "", 0, nil
	}
	if session == nil || h.fileResolver == nil {
		return version, "", 0, errors.New("virtual playback dependencies are unavailable")
	}
	file, err := h.fileResolver.GetByID(ctx, version.FileID)
	if err != nil || file == nil {
		return version, "", 0, errors.New("virtual media file is unavailable")
	}
	resolved, err := h.resolveAndProbeVirtualSource(ctx, file, session.StreamAppUserID, session.ProfileID)
	if err != nil {
		return version, "", 0, err
	}
	return applyVirtualProbeToVersion(version, resolved.file), resolved.uri, resolved.ownerID, nil
}

func (h *PlaybackHandler) resolveAndProbeVirtualSource(ctx context.Context, file *models.MediaFile, userID int, profileID string) (resolvedCompatVirtualSource, error) {
	if !isCompatVirtualFile(file) {
		return resolvedCompatVirtualSource{file: file}, nil
	}

	parsed, _ := url.Parse(file.FilePath)
	hasResult := parsed != nil && strings.TrimSpace(parsed.Query().Get("result")) != ""

	candidates := []VirtualPlaybackStream(nil)
	if hasResult {
		candidates = []VirtualPlaybackStream{{
			URI:                 file.FilePath,
			Resolution:          file.Resolution,
			CodecVideo:          file.CodecVideo,
			CodecAudio:          file.CodecAudio,
			Container:           file.Container,
			HDR:                 mediaFileHDRString(file),
			FileSize:            file.FileSize,
			Bitrate:             file.Bitrate,
			OwnerInstallationID: file.VirtualOwnerInstallationID,
		}}
	} else if h.VirtualPlaybackStreamLister != nil {
		listCtx, cancel := context.WithTimeout(ctx, compatVirtualListTimeout)
		streams, err := h.VirtualPlaybackStreamLister.ListVirtualPlaybackStreams(listCtx, file.FilePath, userID, profileID, file.VirtualOwnerInstallationID)
		cancel()
		if err != nil {
			return resolvedCompatVirtualSource{}, fmt.Errorf("list virtual playback streams: %w", err)
		}
		if filtered := filterCompatVirtualCandidates(file.FilePath, streams); len(filtered) > 0 {
			candidates = filtered
		}
	} else {
		candidates = []VirtualPlaybackStream{{
			URI:                 file.FilePath,
			Resolution:          file.Resolution,
			CodecVideo:          file.CodecVideo,
			CodecAudio:          file.CodecAudio,
			Container:           file.Container,
			HDR:                 mediaFileHDRString(file),
			FileSize:            file.FileSize,
			Bitrate:             file.Bitrate,
			OwnerInstallationID: file.VirtualOwnerInstallationID,
		}}
	}
	if len(candidates) > compatVirtualMaxProbeAttempts {
		candidates = candidates[:compatVirtualMaxProbeAttempts]
	}

	for _, candidate := range candidates {
		uri := strings.TrimSpace(candidate.URI)
		if !isCompatVirtualPath(uri) {
			continue
		}
		ownerID := candidate.OwnerInstallationID
		if ownerID <= 0 {
			ownerID = file.VirtualOwnerInstallationID
		}
		transient := *file
		transient.FilePath = uri
		transient.VirtualOwnerInstallationID = ownerID
		if candidate.Resolution != "" {
			transient.Resolution = candidate.Resolution
		}
		if candidate.CodecVideo != "" {
			transient.CodecVideo = candidate.CodecVideo
		}
		if candidate.CodecAudio != "" {
			transient.CodecAudio = candidate.CodecAudio
		}
		if candidate.Container != "" {
			transient.Container = candidate.Container
		}
		if candidate.HDR != "" {
			transient.HDR = true
		}
		if candidate.FileSize > 0 {
			transient.FileSize = candidate.FileSize
		}
		if candidate.Bitrate > 0 {
			transient.Bitrate = candidate.Bitrate
		}
		mergeCompatCandidateTracks(&transient, candidate)
		return resolvedCompatVirtualSource{file: &transient, uri: uri, ownerID: ownerID}, nil
	}
	return resolvedCompatVirtualSource{}, errors.New("virtual playback provider returned no usable stream")
}

func mergeCompatCandidateTracks(probed *models.MediaFile, candidate VirtualPlaybackStream) {
	if probed == nil {
		return
	}
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

	channels := inferCompatChannelsFromCodec(probed.CodecAudio)
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
	if len(probed.VideoTracks) == 0 {
		hdrStr := strings.ToLower(candidate.HDR)
		isDV := compatVirtualIsDV(hdrStr)
		dvProfile := compatVirtualDVProfile(candidate.HDR)
		videoRange := "SDR"
		videoRangeType := "SDR"
		dovi := ""
		if isDV {
			videoRange = "DolbyVision"
			videoRangeType = "DOVI"
			if dvProfile == 0 {
				dvProfile = 5
			}
			dovi = "Profile " + strconv.Itoa(dvProfile)
		} else if probed.HDR || candidate.HDR != "" {
			videoRange = "HDR"
			videoRangeType = "HDR10"
		}
		probed.VideoTracks = append(probed.VideoTracks, models.VideoTrack{
			Codec:          videoCodec,
			Width:          compatResolutionWidth(probed.Resolution),
			Height:         compatResolutionHeight(probed.Resolution),
			FrameRate:      "23.976",
			BitDepth:       8,
			Bitrate:        probed.Bitrate,
			VideoRange:     videoRange,
			VideoRangeType: videoRangeType,
			DVProfile:      dvProfile,
			DolbyVision:    dovi,
		})
	}
	for i := range probed.VideoTracks {
		if probed.VideoTracks[i].Width == 0 {
			probed.VideoTracks[i].Width = compatResolutionWidth(probed.Resolution)
		}
		if probed.VideoTracks[i].Height == 0 {
			probed.VideoTracks[i].Height = compatResolutionHeight(probed.Resolution)
		}
		if probed.VideoTracks[i].FrameRate == "" {
			probed.VideoTracks[i].FrameRate = "23.976"
		}
		if probed.VideoTracks[i].BitDepth <= 0 {
			probed.VideoTracks[i].BitDepth = 8
		}
		hdrStr := strings.ToLower(candidate.HDR)
		if compatVirtualIsDV(hdrStr) && probed.VideoTracks[i].DVProfile == 0 {
			probed.VideoTracks[i].VideoRange = "DolbyVision"
			probed.VideoTracks[i].VideoRangeType = "DOVI"
			profile := compatVirtualDVProfile(candidate.HDR)
			if profile == 0 {
				profile = 5
			}
			probed.VideoTracks[i].DVProfile = profile
			probed.VideoTracks[i].DolbyVision = "Profile " + strconv.Itoa(profile)
		} else if !compatVirtualIsDV(hdrStr) && (probed.HDR || candidate.HDR != "") &&
			(strings.TrimSpace(probed.VideoTracks[i].VideoRange) == "" ||
				strings.EqualFold(probed.VideoTracks[i].VideoRange, "sdr") ||
				strings.EqualFold(probed.VideoTracks[i].VideoRangeType, "sdr")) {
			probed.VideoTracks[i].VideoRange = "HDR"
			probed.VideoTracks[i].VideoRangeType = "HDR10"
		}
	}
	audioCodec := probed.CodecAudio
	if audioCodec == "" {
		audioCodec = candidate.CodecAudio
	}
	if audioCodec == "" {
		audioCodec = "aac"
	}
	if probed.CodecAudio == "" {
		probed.CodecAudio = audioCodec
	}
	if len(probed.AudioTracks) == 0 {
		probed.AudioTracks = append(probed.AudioTracks, models.AudioTrack{
			Codec:    audioCodec,
			Channels: channels,
			Default:  true,
		})
	}
	for i := range probed.AudioTracks {
		if probed.AudioTracks[i].Codec == "" {
			probed.AudioTracks[i].Codec = audioCodec
		}
		if probed.AudioTracks[i].Channels == 0 {
			probed.AudioTracks[i].Channels = channels
		}
	}

	if len(candidate.AudioLanguages) > 0 {
		existing := make(map[string]bool, len(probed.AudioTracks))
		for _, track := range probed.AudioTracks {
			if language := strings.TrimSpace(track.Language); language != "" {
				existing[strings.ToLower(language)] = true
			}
		}
		for _, language := range candidate.AudioLanguages {
			language = strings.TrimSpace(language)
			if language == "" || existing[strings.ToLower(language)] {
				continue
			}
			existing[strings.ToLower(language)] = true
			assigned := false
			for i := range probed.AudioTracks {
				if strings.TrimSpace(probed.AudioTracks[i].Language) == "" {
					probed.AudioTracks[i].Language = language
					assigned = true
					break
				}
			}
			if !assigned {
				probed.AudioTracks = append(probed.AudioTracks, models.AudioTrack{
					Language: language,
					Codec:    audioCodec,
					Channels: channels,
				})
			}
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

	if len(candidate.SubtitleLanguages) > 0 {
		existing := make(map[string]bool, len(probed.SubtitleTracks))
		for _, track := range probed.SubtitleTracks {
			if language := strings.TrimSpace(track.Language); language != "" {
				existing[strings.ToLower(language)] = true
			}
		}
		for _, language := range candidate.SubtitleLanguages {
			language = strings.TrimSpace(language)
			if language == "" || existing[strings.ToLower(language)] {
				continue
			}
			existing[strings.ToLower(language)] = true
			probed.SubtitleTracks = append(probed.SubtitleTracks, models.SubtitleTrack{
				Index:    len(probed.SubtitleTracks),
				Language: language,
				Codec:    "srt",
				Title:    language,
			})
		}
	}
}

var compatVirtualDVProfileRegex = regexp.MustCompile(`(?i)(?:profile|dovi|dv|dolby\s*vision)\s*[-._:]?\s*0*([1-9]|1\d|20)(?:[.]\d+)?(?:[^a-z0-9]|$)`)

func compatVirtualDVProfile(raw string) int {
	lower := strings.ToLower(strings.TrimSpace(raw))
	if matches := compatVirtualDVProfileRegex.FindStringSubmatch(lower); len(matches) > 1 {
		if profile, err := strconv.Atoi(matches[1]); err == nil && profile > 0 && profile <= 20 {
			return profile
		}
	}
	return 0
}

func compatVirtualIsDV(raw string) bool {
	lower := strings.ToLower(strings.TrimSpace(raw))
	return strings.Contains(lower, "dolby vision") || strings.Contains(lower, "dovi") ||
		lower == "dv" || strings.HasPrefix(lower, "dv ") || strings.HasSuffix(lower, " dv") || strings.Contains(lower, " dv ") ||
		compatVirtualDVProfileMarker(lower)
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

func compatVirtualDVProfileMarker(raw string) bool {
	for _, profile := range []string{"profile 5", "profile 7", "profile 8", "dv5", "dv7", "dv8"} {
		if strings.Contains(raw, profile) {
			return true
		}
	}
	return false
}

//nolint:goconst
func inferCompatChannelsFromCodec(codec string) int {
	switch strings.ToLower(codec) {
	case "atmos":
		return 8
	case "truehd", "dts-hd", "dts", "eac3", "ac3":
		return 6
	default:
		return 2
	}
}

//nolint:goconst
func compatResolutionWidth(label string) int {
	switch strings.ToLower(label) {
	case "2160p", "4k":
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

//nolint:goconst
func compatResolutionHeight(label string) int {
	switch strings.ToLower(label) {
	case "2160p", "4k":
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

func filterCompatVirtualCandidates(baseURI string, streams []VirtualPlaybackStream) []VirtualPlaybackStream {
	base, err := url.Parse(baseURI)
	if err != nil {
		return nil
	}
	profile := strings.TrimSpace(base.Query().Get("profile"))
	result := strings.TrimSpace(base.Query().Get("result"))
	seen := make(map[string]struct{}, len(streams))
	out := make([]VirtualPlaybackStream, 0, len(streams))
	for _, stream := range streams {
		if len(out) >= compatVirtualMaxProbeAttempts {
			break
		}
		uri := strings.TrimSpace(stream.URI)
		if !isCompatVirtualPath(uri) {
			continue
		}
		parsed, parseErr := url.Parse(uri)
		if parseErr != nil || !sameCompatVirtualIdentity(base, parsed) {
			continue
		}
		query := parsed.Query()
		if result != "" && query.Get("result") != result {
			continue
		}
		if profile != "" && !strings.EqualFold(strings.TrimSpace(stream.Label), profile) && !strings.EqualFold(strings.TrimSpace(query.Get("profile")), profile) {
			continue
		}
		key := strings.ToLower(uri)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, stream)
	}
	return out
}

func sameCompatVirtualIdentity(a, b *url.URL) bool {
	return a != nil && b != nil && strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host) && a.EscapedPath() == b.EscapedPath()
}

func applyVirtualProbeToVersion(version catalog.FileVersion, file *models.MediaFile) catalog.FileVersion {
	if file == nil {
		return version
	}
	version.FilePath = file.FilePath
	version.Resolution = file.Resolution
	version.CodecVideo = file.CodecVideo
	version.CodecAudio = file.CodecAudio
	version.HDR = file.HDR
	version.Container = file.Container
	version.FileSize = file.FileSize
	version.Duration = file.Duration
	version.Bitrate = file.Bitrate
	version.VideoTracks = append([]models.VideoTrack(nil), file.VideoTracks...)
	version.AudioTracks = append([]models.AudioTrack(nil), file.AudioTracks...)
	version.SubtitleTracks = make([]catalog.VersionSubtitleTrack, 0, len(file.SubtitleTracks))
	for _, track := range file.SubtitleTracks {
		version.SubtitleTracks = append(version.SubtitleTracks, catalog.VersionSubtitleTrack{
			Index: track.Index, Language: track.Language, Codec: track.Codec,
			Title: track.Title, EmbeddedTitle: track.EmbeddedTitle, Resolution: track.Resolution,
			Forced: track.Forced, Default: track.Default, HearingImpaired: track.HearingImpaired,
			External: track.External, FileName: track.FileName,
		})
	}
	return version
}

func virtualMediaFileForSource(file *models.MediaFile, source PlaybackMediaSource) *models.MediaFile {
	if file == nil || !isCompatVirtualSource(source) {
		return file
	}
	copy := *file
	copy.FilePath = source.VirtualSourceURI
	copy.VirtualOwnerInstallationID = source.VirtualSourceOwnerInstallationID
	copy.Resolution = source.Version.Resolution
	copy.CodecVideo = source.Version.CodecVideo
	copy.CodecAudio = source.Version.CodecAudio
	copy.HDR = source.Version.HDR
	copy.Container = source.Version.Container
	copy.FileSize = source.Version.FileSize
	copy.Duration = source.Version.Duration
	copy.Bitrate = source.Version.Bitrate
	copy.VideoTracks = append([]models.VideoTrack(nil), source.Version.VideoTracks...)
	copy.AudioTracks = append([]models.AudioTrack(nil), source.Version.AudioTracks...)
	copy.SubtitleTracks = make([]models.SubtitleTrack, 0, len(source.Version.SubtitleTracks))
	for _, track := range source.Version.SubtitleTracks {
		if track.External {
			continue
		}
		copy.SubtitleTracks = append(copy.SubtitleTracks, models.SubtitleTrack{
			Index: track.Index, Language: track.Language, Codec: track.Codec,
			Title: track.Title, EmbeddedTitle: track.EmbeddedTitle, Resolution: track.Resolution,
			Forced: track.Forced, Default: track.Default, HearingImpaired: track.HearingImpaired,
		})
	}
	return &copy
}

func (h *PlaybackHandler) resolveVirtualTransport(ctx context.Context, session *Session, source PlaybackMediaSource, forceRefresh bool) (string, error) {
	if session == nil {
		return "", errors.New("compat playback session is unavailable")
	}
	return h.resolveVirtualTransportForIdentity(ctx, session.StreamAppUserID, session.ProfileID, source, forceRefresh)
}

func (h *PlaybackHandler) resolveVirtualTransportForIdentity(ctx context.Context, userID int, profileID string, source PlaybackMediaSource, forceRefresh bool) (string, error) {
	uri := strings.TrimSpace(source.VirtualSourceURI)
	if !isCompatVirtualPath(uri) {
		return "", errors.New("virtual playback source is not bound")
	}
	if h.VirtualMediaResolver == nil {
		return "", errors.New("virtual playback resolver is not configured")
	}
	if forceRefresh && h.VirtualMediaRefreshResolver != nil {
		return h.VirtualMediaRefreshResolver.RefreshVirtualMedia(ctx, uri, source.VirtualSourceOwnerInstallationID, userID, profileID)
	}
	return h.VirtualMediaResolver.ResolveVirtualMedia(ctx, uri, source.VirtualSourceOwnerInstallationID, userID, profileID)
}

func (h *PlaybackHandler) registerVirtualInput(ctx context.Context, session *Session, source PlaybackMediaSource, forceRefresh bool) (string, func(), error) {
	if session == nil {
		return "", nil, errors.New("compat playback session is unavailable")
	}
	return h.registerVirtualInputForIdentity(ctx, session.StreamAppUserID, session.ProfileID, source, forceRefresh)
}

func (h *PlaybackHandler) registerVirtualInputForIdentity(ctx context.Context, userID int, profileID string, source PlaybackMediaSource, forceRefresh bool) (string, func(), error) {
	resolved, err := h.resolveVirtualTransportForIdentity(ctx, userID, profileID, source, forceRefresh)
	if err != nil {
		return "", nil, err
	}
	if h.RemoteStreamRelay == nil {
		return "", nil, errors.New("remote stream relay is not configured")
	}
	insecure := h.AllowInsecureVirtual != nil && h.AllowInsecureVirtual(source.VirtualSourceOwnerInstallationID)
	relayURL, cleanup, err := registerRemoteStreamInput(ctx, h.RemoteStreamRelay, resolved, insecure)
	if err != nil {
		return "", nil, err
	}
	return relayURL, cleanup, nil
}

func (h *PlaybackHandler) serveVirtualDirect(w http.ResponseWriter, r *http.Request, session *Session, source PlaybackMediaSource) error {
	if h.RemoteStreamRelay == nil {
		return errors.New("remote stream relay is not configured")
	}
	resolved, err := h.resolveVirtualTransport(r.Context(), session, source, false)
	if err != nil {
		return err
	}
	streamWriter := httpstream.NewRollingDeadlineWriter(w)
	err = h.proxyVirtualStream(streamWriter, r, source, resolved)
	var proxyErr *remotestream.ProxyError
	if errors.As(err, &proxyErr) && proxyErr.Started {
		// Headers or media bytes already reached the client. The connection error
		// itself is the response; appending a JSON error would corrupt the stream.
		return nil
	}
	if err == nil || !remotestream.RetryableBeforeResponse(err) || h.VirtualMediaRefreshResolver == nil {
		return err
	}
	resolved, refreshErr := h.resolveVirtualTransport(r.Context(), session, source, true)
	if refreshErr != nil {
		return refreshErr
	}
	err = h.proxyVirtualStream(streamWriter, r, source, resolved)
	if errors.As(err, &proxyErr) && proxyErr.Started {
		return nil
	}
	return err
}

// proxyVirtualStream routes the resolved URL through the strict SSRF-protected
// proxy, or through the insecure proxy when the owning plugin installation has
// explicitly enabled allow_insecure_http for private/local hosts.
func (h *PlaybackHandler) proxyVirtualStream(w http.ResponseWriter, r *http.Request, source PlaybackMediaSource, resolved string) error {
	if h.AllowInsecureVirtual != nil && h.AllowInsecureVirtual(source.VirtualSourceOwnerInstallationID) {
		return h.RemoteStreamRelay.ProxyInsecure(w, r, resolved)
	}
	return h.RemoteStreamRelay.Proxy(w, r, resolved)
}

func virtualDownloadName(version catalog.FileVersion) string {
	name := strings.TrimSpace(version.FileName)
	if name == "" {
		name = "stream"
	}
	if filepath.Ext(name) == "" && strings.TrimSpace(version.Container) != "" {
		name += "." + strings.ToLower(strings.TrimSpace(version.Container))
	}
	return name
}
