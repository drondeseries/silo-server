// Package remuxdb provides an HTTP client to RemuxDB (https://remuxdb.1632022.xyz),
// a open crowdsourced stream metadata database for movies and TV shows.
package remuxdb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

const DefaultBaseURL = "https://remuxdb.1632022.xyz"

// Client interacts with the RemuxDB API.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	Token      string
	ClientID   string
}

// NewClient returns a Client with defaults.
func NewClient(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		Token:    token,
		ClientID: "silo-server",
	}
}

// ExternalIDs represents external catalog IDs for RemuxDB submissions/fetches.
type ExternalIDs struct {
	IMDbID string `json:"imdb_id,omitempty"`
	TMDbID int64  `json:"tmdb_id,omitempty"`
	TVDbID int64  `json:"tvdb_id,omitempty"`
}

// TrackDetail represents a video, audio, or subtitle track returned by RemuxDB.
type TrackDetail struct {
	Kind              string  `json:"kind"`
	Index             int     `json:"idx"`
	IsDefault         bool    `json:"is_default,omitempty"`
	IsForced          bool    `json:"is_forced,omitempty"`
	IsHearingImpaired bool    `json:"is_hearing_impaired,omitempty"`
	IsExternal        bool    `json:"is_external,omitempty"`
	IsAnamorphic      bool    `json:"is_anamorphic,omitempty"`
	HDR10PlusPresent  bool    `json:"hdr10_plus_present,omitempty"`
	Codec             string  `json:"codec,omitempty"`
	Language          string  `json:"language,omitempty"`
	Title             string  `json:"title,omitempty"`
	BitRate           int64   `json:"bit_rate,omitempty"`
	BitDepth          int     `json:"bit_depth,omitempty"`
	PixelFormat       string  `json:"pixel_format,omitempty"`
	Profile           string  `json:"profile,omitempty"`
	Level             int     `json:"level,omitempty"`
	Width             int     `json:"width,omitempty"`
	Height            int     `json:"height,omitempty"`
	FPS               float64 `json:"fps,omitempty"`
	AspectRatio       string  `json:"aspect_ratio,omitempty"`
	ColorPrimaries    string  `json:"color_primaries,omitempty"`
	ColorRange        string  `json:"color_range,omitempty"`
	ColorSpace        string  `json:"color_space,omitempty"`
	ColorTransfer     string  `json:"color_transfer,omitempty"`
	DVProfile         int     `json:"dv_profile,omitempty"`
	Channels          int     `json:"channels,omitempty"`
	SampleRate        int     `json:"sample_rate,omitempty"`
	ChannelLayout     string  `json:"channel_layout,omitempty"`
}

// MediaInfo represents one stream variant entry returned by GET /api/media/info.
type MediaInfo struct {
	ContentHash string        `json:"content_hash,omitempty"`
	Container   string        `json:"container,omitempty"`
	Duration    float64       `json:"duration,omitempty"`
	Size        int64         `json:"size,omitempty"`
	Bitrate     int64         `json:"bitrate,omitempty"`
	Tracks      []TrackDetail `json:"tracks,omitempty"`
}

// FetchProbe fetches stream metadata versions from RemuxDB by IMDb ID.
func (c *Client) FetchProbe(ctx context.Context, imdbID string, season, episode *int) ([]MediaInfo, error) {
	if imdbID == "" {
		return nil, nil
	}
	u, err := url.Parse(c.BaseURL + "/api/media/info")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("imdb_id", imdbID)
	if season != nil {
		q.Set("season", strconv.Itoa(*season))
	}
	if episode != nil {
		q.Set("episode", strconv.Itoa(*episode))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	clientID := c.ClientID
	if clientID == "" {
		clientID = "silo-server"
	}
	req.Header.Set("x-client-id", clientID)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("remuxdb fetch status %d", resp.StatusCode)
	}

	var results []MediaInfo
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, err
	}
	return results, nil
}

// MediaInfoPayload represents the payload to submit probed stream info to POST /api/mediainfo.
type MediaInfoPayload struct {
	Kind        string       `json:"kind"`
	Filename    string       `json:"filename"`
	Container   string       `json:"container"`
	Size        int64        `json:"size"`
	Duration    float64      `json:"duration"`
	Bitrate     int64        `json:"bitrate,omitempty"`
	Season      *int         `json:"season,omitempty"`
	Episode     *int         `json:"episode,omitempty"`
	ExternalIDs *ExternalIDs `json:"external_ids,omitempty"`
	Tracks      []any        `json:"tracks"`
}

// SubmitMediaInfo posts probed media information to RemuxDB to contribute to the crowdsourced DB.
func (c *Client) SubmitMediaInfo(ctx context.Context, payload MediaInfoPayload) error {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	u := c.BaseURL + "/api/mediainfo"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	clientID := c.ClientID
	if clientID == "" {
		clientID = "silo-server"
	}
	req.Header.Set("x-client-id", clientID)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("remuxdb submit status %d", resp.StatusCode)
	}
	return nil
}

// PopulateMediaFileFromRemuxDB converts RemuxDB MediaInfo tracks into a models.MediaFile.
func PopulateMediaFileFromRemuxDB(info MediaInfo, file *models.MediaFile) bool {
	if file == nil || len(info.Tracks) == 0 {
		return false
	}
	if info.Container != "" {
		file.Container = info.Container
	}
	if info.Duration > 0 {
		file.Duration = int(info.Duration)
	}
	if info.Bitrate > 0 {
		file.Bitrate = int(info.Bitrate / 1000)
	}
	if info.Size > 0 {
		file.FileSize = info.Size
	}

	videoTracks := make([]models.VideoTrack, 0)
	audioTracks := make([]models.AudioTrack, 0)
	subTracks := make([]models.SubtitleTrack, 0)

	for _, t := range info.Tracks {
		switch strings.ToLower(t.Kind) {
		case "video":
			videoRange := "SDR"
			videoRangeType := "SDR"
			if t.ColorTransfer == "smpte2084" || t.DVProfile > 0 || t.HDR10PlusPresent {
				videoRange = "HDR"
				if t.DVProfile > 0 {
					videoRangeType = "DOVI"
				} else if t.HDR10PlusPresent {
					videoRangeType = "HDR10Plus"
				} else {
					videoRangeType = "HDR10"
				}
			}
			doviStr := ""
			if t.DVProfile > 0 {
				doviStr = "true"
			}
			vt := models.VideoTrack{
				Title:          t.Title,
				Codec:          t.Codec,
				DolbyVision:    doviStr,
				DVProfile:      t.DVProfile,
				HDR10Plus:      t.HDR10PlusPresent,
				Profile:        t.Profile,
				Level:          t.Level,
				Width:          t.Width,
				Height:         t.Height,
				AspectRatio:    t.AspectRatio,
				FrameRate:      fmt.Sprintf("%.3f", t.FPS),
				Bitrate:        int(t.BitRate / 1000),
				BitDepth:       t.BitDepth,
				VideoRange:     videoRange,
				VideoRangeType: videoRangeType,
				ColorSpace:     t.ColorSpace,
				ColorTransfer:  t.ColorTransfer,
				ColorPrimaries: t.ColorPrimaries,
			}
			videoTracks = append(videoTracks, vt)

			if file.CodecVideo == "" {
				file.CodecVideo = t.Codec
			}
			if file.Resolution == "" {
				file.Resolution = resolutionLabel(t.Height)
			}
			if t.DVProfile > 0 || t.HDR10PlusPresent || videoRange == "HDR" {
				file.HDR = true
			}

		case "audio":
			at := models.AudioTrack{
				Title:    t.Title,
				Language: t.Language,
				Codec:    t.Codec,
				Channels: t.Channels,
				Layout:   t.ChannelLayout,
				Bitrate:  int(t.BitRate / 1000),
				Default:  t.IsDefault,
			}
			audioTracks = append(audioTracks, at)
			if file.CodecAudio == "" {
				file.CodecAudio = t.Codec
			}
			if file.AudioChannels == 0 {
				file.AudioChannels = t.Channels
			}

		case "subtitle":
			st := models.SubtitleTrack{
				Title:           t.Title,
				Language:        t.Language,
				Codec:           t.Codec,
				Default:         t.IsDefault,
				Forced:          t.IsForced,
				HearingImpaired: t.IsHearingImpaired,
			}
			subTracks = append(subTracks, st)
		}
	}

	if len(videoTracks) > 0 {
		file.VideoTracks = videoTracks
	}
	if len(audioTracks) > 0 {
		file.AudioTracks = audioTracks
	}
	if len(subTracks) > 0 {
		file.SubtitleTracks = subTracks
	}
	return len(videoTracks) > 0
}

func resolutionLabel(h int) string {
	switch {
	case h >= 2160:
		return "2160p"
	case h >= 1080:
		return "1080p"
	case h >= 720:
		return "720p"
	case h >= 480:
		return "480p"
	default:
		return "SD"
	}
}

// ExtractIMDbID parses an IMDb ID (e.g. "tt0133093") from a content ID or file path.
func ExtractIMDbID(s string) string {
	lower := strings.ToLower(s)
	idx := strings.Index(lower, "tt")
	if idx == -1 {
		return ""
	}
	sub := lower[idx:]
	end := 2
	for end < len(sub) && sub[end] >= '0' && sub[end] <= '9' {
		end++
	}
	if end >= 7 { // IMDb IDs are tt + 5 to 8 digits (e.g. tt0133093, tt19035928)
		return sub[:end]
	}
	return ""
}

// BuildMediaInfoPayload converts a probed models.MediaFile into a RemuxDB submission payload.
func BuildMediaInfoPayload(file *models.MediaFile, imdbID string) (MediaInfoPayload, bool) {
	if file == nil || (len(file.VideoTracks) == 0 && len(file.AudioTracks) == 0) {
		return MediaInfoPayload{}, false
	}
	if imdbID == "" {
		imdbID = ExtractIMDbID(file.ContentID)
		if imdbID == "" {
			imdbID = ExtractIMDbID(file.FilePath)
		}
	}
	if imdbID == "" {
		return MediaInfoPayload{}, false
	}

	filename := file.FilePath
	if u, err := url.Parse(file.FilePath); err == nil && u.Path != "" {
		parts := strings.Split(u.Path, "/")
		if len(parts) > 0 && parts[len(parts)-1] != "" {
			filename = parts[len(parts)-1]
		}
	}

	tracks := make([]any, 0)
	for i, vt := range file.VideoTracks {
		tracks = append(tracks, map[string]any{
			"kind":               "video",
			"idx":                i,
			"codec":              vt.Codec,
			"width":              vt.Width,
			"height":             vt.Height,
			"bit_rate":           vt.Bitrate * 1000,
			"bit_depth":          vt.BitDepth,
			"pixel_format":       vt.PixelFormat,
			"profile":            vt.Profile,
			"title":              vt.Title,
			"color_primaries":    vt.ColorPrimaries,
			"color_range":        vt.ColorRange,
			"color_space":        vt.ColorSpace,
			"color_transfer":     vt.ColorTransfer,
			"aspect_ratio":       vt.AspectRatio,
			"dv_profile":         vt.DVProfile,
			"dv_rpu_present":     vt.DolbyVision != "" && vt.DolbyVision != "false",
			"hdr10_plus_present": vt.HDR10Plus,
		})
	}
	for i, at := range file.AudioTracks {
		tracks = append(tracks, map[string]any{
			"kind":           "audio",
			"idx":            i,
			"codec":          at.Codec,
			"channels":       at.Channels,
			"bit_rate":       at.Bitrate * 1000,
			"channel_layout": at.Layout,
			"title":          at.Title,
			"language":       at.Language,
			"is_default":     at.Default,
		})
	}
	for i, st := range file.SubtitleTracks {
		tracks = append(tracks, map[string]any{
			"kind":                "subtitle",
			"idx":                 i,
			"codec":               st.Codec,
			"title":               st.Title,
			"language":            st.Language,
			"is_default":          st.Default,
			"is_forced":           st.Forced,
			"is_hearing_impaired": st.HearingImpaired,
		})
	}

	kind := "movie"
	if file.EpisodeNumber > 0 || file.SeasonNumber > 0 {
		kind = "episode"
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

	return MediaInfoPayload{
		Kind:      kind,
		Filename:  filename,
		Container: file.Container,
		Size:      file.FileSize,
		Duration:  float64(file.Duration),
		Bitrate:   int64(file.Bitrate * 1000),
		Season:    seasonPtr,
		Episode:   epPtr,
		ExternalIDs: &ExternalIDs{
			IMDbID: imdbID,
		},
		Tracks: tracks,
	}, true
}
