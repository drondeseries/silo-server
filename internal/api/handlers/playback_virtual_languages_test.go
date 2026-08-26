package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

func TestVisibleVirtualPlaybackStreamsHidesAlternatesOnlyWhenMarked(t *testing.T) {
	streams := []VirtualPlaybackStream{{URI: "virtual://movie/1?result=one", Visible: true, VisibilitySpecified: true}, {URI: "virtual://movie/1?result=two", Visible: false, VisibilitySpecified: true}}
	visible := visibleVirtualPlaybackStreams(streams)
	if len(visible) != 1 || visible[0].URI != streams[0].URI {
		t.Fatalf("visible streams = %#v, want only primary", visible)
	}
	legacy := visibleVirtualPlaybackStreams([]VirtualPlaybackStream{{URI: "one"}, {URI: "two"}})
	if len(legacy) != 2 {
		t.Fatalf("unmarked streams = %#v, want both candidates", legacy)
	}
}

func TestMergeVirtualCandidateLanguagesSynthesizesAudioAndSubtitles(t *testing.T) {
	probed := &models.MediaFile{
		VideoTracks:    []models.VideoTrack{{Codec: "hevc", Width: 3840, Height: 2160}},
		SubtitleTracks: []models.SubtitleTrack{{Index: 0, Language: "eng", Codec: "subrip"}},
	}
	candidate := VirtualPlaybackStream{
		AudioLanguages:    []string{"ita", "ENG", "ita"},
		SubtitleLanguages: []string{"eng", "ger"},
	}

	mergeVirtualCandidateTracks(probed, candidate)

	if len(probed.AudioTracks) != 2 {
		t.Fatalf("audio tracks: got %d, want 2", len(probed.AudioTracks))
	}
	if got := probed.AudioTracks[0].Language; got != "ita" {
		t.Errorf("audio[0].language: got %q, want ita", got)
	}
	if got := probed.AudioTracks[1].Language; got != "ENG" {
		t.Errorf("audio[1].language: got %q, want ENG", got)
	}
	// Duplicate "ita" in the candidate must not append a third track.
	if !probed.AudioTracks[0].Default {
		t.Error("first synthesized audio track should be marked default")
	}

	if len(probed.SubtitleTracks) != 2 {
		t.Fatalf("subtitle tracks: got %d, want 2", len(probed.SubtitleTracks))
	}
	if got := probed.SubtitleTracks[1].Language; got != "ger" {
		t.Errorf("subtitle[1].language: got %q, want ger", got)
	}
	if got := probed.SubtitleTracks[1].Index; got != 1 {
		t.Errorf("subtitle[1].index: got %d, want 1", got)
	}
	// The pre-existing probed "eng" subtitle must not be duplicated.
	seenEng := 0
	for _, st := range probed.SubtitleTracks {
		if st.Language == "eng" {
			seenEng++
		}
	}
	if seenEng != 1 {
		t.Errorf("eng subtitle duplicated: got %d entries, want 1", seenEng)
	}
}

func TestMergeVirtualCandidateTracksPreservesDVProfile(t *testing.T) {
	probed := &models.MediaFile{Resolution: "2160p", CodecVideo: "hevc"}
	mergeVirtualCandidateTracks(probed, VirtualPlaybackStream{HDR: "Dolby Vision Profile 5"})
	if len(probed.VideoTracks) != 1 || probed.VideoTracks[0].DVProfile != 5 {
		t.Fatalf("DV metadata = %#v", probed.VideoTracks)
	}
}

func TestMergeVirtualCandidateTracksDoesNotInferDVFromGenericHDR(t *testing.T) {
	probed := &models.MediaFile{Resolution: "2160p", CodecVideo: "hevc"}
	mergeVirtualCandidateTracks(probed, VirtualPlaybackStream{HDR: "true"})
	if len(probed.VideoTracks) != 1 || probed.VideoTracks[0].DVProfile != 0 || probed.VideoTracks[0].DolbyVision != "" {
		t.Fatalf("generic HDR became DV: %#v", probed.VideoTracks)
	}
}

func TestMergeVirtualCandidateTracksRepairsStaleSDRRange(t *testing.T) {
	probed := &models.MediaFile{
		HDR:        true,
		Resolution: "2160p",
		CodecVideo: "hevc",
		VideoTracks: []models.VideoTrack{{
			VideoRange:     "SDR",
			VideoRangeType: "SDR",
		}},
	}

	mergeVirtualCandidateTracks(probed, VirtualPlaybackStream{HDR: "true"})

	if got := probed.VideoTracks[0].VideoRange; got != "HDR" {
		t.Fatalf("video range = %q, want HDR", got)
	}
	if got := probed.VideoTracks[0].VideoRangeType; got != "HDR10" {
		t.Fatalf("video range type = %q, want HDR10", got)
	}
}

func TestMergeVirtualCandidateLanguagesKeepsProbedTracks(t *testing.T) {
	probed := &models.MediaFile{
		AudioTracks: []models.AudioTrack{{Language: "eng", Codec: "aac", Channels: 2}},
	}
	candidate := VirtualPlaybackStream{
		AudioLanguages: []string{"eng", "ita"},
	}

	mergeVirtualCandidateTracks(probed, candidate)

	if len(probed.AudioTracks) != 2 {
		t.Fatalf("audio tracks: got %d, want 2", len(probed.AudioTracks))
	}
	if got := probed.AudioTracks[0].Codec; got != "aac" {
		t.Errorf("probed track codec overwritten: got %q, want aac", got)
	}
	if got := probed.AudioTracks[0].Channels; got != 2 {
		t.Errorf("probed track channels overwritten: got %d, want 2", got)
	}
	if got := probed.AudioTracks[1].Language; got != "ita" {
		t.Errorf("audio[1].language: got %q, want ita", got)
	}
}

func TestMergeVirtualCandidateLanguagesNilFile(t *testing.T) {
	mergeVirtualCandidateTracks(nil, VirtualPlaybackStream{AudioLanguages: []string{"eng"}})
	// Must not panic.
}

func TestMergeVirtualCandidateLanguagesSkipsReleaseMarkers(t *testing.T) {
	probed := &models.MediaFile{}
	candidate := VirtualPlaybackStream{
		AudioLanguages:    []string{"ITA", "ENG", "MULTI", "DUAL", "multi"},
		SubtitleLanguages: []string{"eng", "ger", "MULTI"},
	}

	mergeVirtualCandidateTracks(probed, candidate)

	gotAudio := make([]string, 0, len(probed.AudioTracks))
	for _, t := range probed.AudioTracks {
		gotAudio = append(gotAudio, t.Language)
	}
	for _, want := range []string{"ITA", "ENG"} {
		found := false
		for _, got := range gotAudio {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("audio languages missing %q: got %v", want, gotAudio)
		}
	}
	for _, got := range gotAudio {
		if got == "MULTI" || got == "DUAL" {
			t.Errorf("release marker %q leaked into audio tracks: %v", got, gotAudio)
		}
	}
	if len(probed.AudioTracks) != 2 {
		t.Errorf("audio tracks: got %d, want 2 (ITA, ENG)", len(probed.AudioTracks))
	}

	gotSubs := make([]string, 0, len(probed.SubtitleTracks))
	for _, st := range probed.SubtitleTracks {
		gotSubs = append(gotSubs, st.Language)
	}
	if len(probed.SubtitleTracks) != 2 {
		t.Errorf("subtitle tracks: got %v, want [eng ger]", gotSubs)
	}
	for _, got := range gotSubs {
		if got == "MULTI" {
			t.Errorf("release marker MULTI leaked into subtitle tracks: %v", gotSubs)
		}
	}
}

func TestIsRealVirtualLanguageTag(t *testing.T) {
	for _, valid := range []string{"ENG", "eng", "ITA", "ger", "ara", "fil", "en", "zh-Hans"} {
		if !isRealVirtualLanguageTag(valid) {
			t.Errorf("isRealVirtualLanguageTag(%q) = false, want true", valid)
		}
	}
	for _, marker := range []string{"MULTI", "multi", "DUAL", "dual", "xx", "UNKNOWN", ""} {
		if isRealVirtualLanguageTag(marker) {
			t.Errorf("isRealVirtualLanguageTag(%q) = true, want false", marker)
		}
	}
}

func TestVirtualDVMetadataRobustProfileExtraction(t *testing.T) {
	cases := []struct {
		raw         string
		wantDV      bool
		wantProfile int
	}{
		{raw: "Dolby Vision Profile 8.1", wantDV: true, wantProfile: 8},
		{raw: "Dolby Vision Profile 5", wantDV: true, wantProfile: 5},
		{raw: "Dolby Vision Profile 7", wantDV: true, wantProfile: 7},
		{raw: "dv5", wantDV: true, wantProfile: 5},
		{raw: "dv 7", wantDV: true, wantProfile: 7},
		{raw: "dovi 08.06", wantDV: true, wantProfile: 8},
		{raw: "Dolby Vision 5", wantDV: true, wantProfile: 5},
		{raw: "4K Dolby Vision", wantDV: true, wantProfile: 0},
		{raw: "Dolby Vision 4K", wantDV: true, wantProfile: 0},
		{raw: "Dolby Vision 2160p", wantDV: true, wantProfile: 0},
		{raw: "DV 1080p", wantDV: true, wantProfile: 0},
		{raw: "Dolby Vision 10bit", wantDV: true, wantProfile: 0},
		{raw: "HDR10", wantDV: false, wantProfile: 0},
		{raw: "DVD", wantDV: false, wantProfile: 0},
	}
	for _, tc := range cases {
		gotDV, gotProf := virtualDVMetadata(tc.raw)
		if gotDV != tc.wantDV || gotProf != tc.wantProfile {
			t.Errorf("virtualDVMetadata(%q) = (%v, %d), want (%v, %d)", tc.raw, gotDV, gotProf, tc.wantDV, tc.wantProfile)
		}
	}
}

func TestMaxVirtualFailoverAttemptsConfigurable(t *testing.T) {
	// Nil / default
	var h *PlaybackHandler
	if got := h.maxVirtualFailoverAttempts(nil); got != 5 {
		t.Fatalf("nil handler max attempts = %d, want 5", got)
	}

	h = &PlaybackHandler{}
	if got := h.maxVirtualFailoverAttempts(nil); got != 5 {
		t.Fatalf("empty handler max attempts = %d, want 5", got)
	}

	// From static config
	h = &PlaybackHandler{
		PlaybackConfig: func() config.PlaybackConfig {
			return config.PlaybackConfig{MaxVirtualFailoverAttempts: 12}
		},
	}
	if got := h.maxVirtualFailoverAttempts(nil); got != 12 {
		t.Fatalf("config max attempts = %d, want 12", got)
	}

	// From dynamic settings repo
	h = &PlaybackHandler{
		SettingsRepo: &fakeServerSettingsStore{
			values: map[string]string{
				"playback.max_virtual_failover_attempts": "8",
			},
		},
		PlaybackConfig: func() config.PlaybackConfig {
			return config.PlaybackConfig{MaxVirtualFailoverAttempts: 12}
		},
	}
	if got := h.maxVirtualFailoverAttempts(nil); got != 8 {
		t.Fatalf("dynamic settings max attempts = %d, want 8", got)
	}
}

func TestIsUnplayableVirtualURI(t *testing.T) {
	cases := []struct {
		uri  string
		want bool
	}{
		{"virtual://series/tt11198330", true},
		{"virtual://series/tvdb/371572", true},
		{"virtual://show/tt11198330", true},
		{"virtual://series/tt11198330/3/2", false},
		{"virtual://series/tvdb/371572/3/2", false},
		{"virtual://movie/tt11198330", false},
		{"virtual://movie/tmdb/12345", false},
		{"/var/media/movie.mkv", false},
	}
	for _, tc := range cases {
		if got := isUnplayableVirtualURI(tc.uri); got != tc.want {
			t.Errorf("isUnplayableVirtualURI(%q) = %v, want %v", tc.uri, got, tc.want)
		}
	}
}

func TestVirtualCandidateLookupUsesStableEpisodeIdentity(t *testing.T) {
	calledCandidateLookup := false
	calledContentLookup := false

	h := &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) (string, error) {
			return "https://provider.example/stream.mp4", nil
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{
				{URI: "virtual://series/tt11198330/3/2?result=fresh123", CodecVideo: "h264", Resolution: "1080p"},
			}, nil
		}),
		VirtualCandidateFileLookup: func(ctx context.Context, path, contentID, episodeID string, ownerInstallationID int) (*models.MediaFile, error) {
			calledCandidateLookup = true
			if path != "virtual://series/tt11198330/3/2" || episodeID != "episode-tvdb-371572-3-2" {
				t.Fatalf("VirtualCandidateFileLookup called with path=%q episode=%q", path, episodeID)
			}
			return &models.MediaFile{ID: 5323060, MediaFolderID: 32, FilePath: path, EpisodeID: episodeID}, nil
		},
		VirtualContentFileLookup: func(ctx context.Context, contentID string) (*models.MediaFile, error) {
			calledContentLookup = true
			return &models.MediaFile{ID: 5293604, MediaFolderID: 32, FilePath: "virtual://series/tt11198330", ContentID: contentID}, nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	episodeFile := &models.MediaFile{
		ID:        0,
		ContentID: "series-tvdb-371572",
		EpisodeID: "episode-tvdb-371572-3-2",
		FilePath:  "virtual://series/tt11198330/3/2",
	}

	resolved, err := h.resolveVirtualPlaybackSource(req, episodeFile, "profile-1", false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource failed: %v", err)
	}

	if !calledCandidateLookup {
		t.Fatal("expected VirtualCandidateFileLookup to be called for episode file")
	}
	if calledContentLookup {
		t.Fatal("VirtualContentFileLookup should NOT be called when EpisodeID is present")
	}
	if resolved.File.ID != 5323060 {
		t.Fatalf("resolved file ID = %d, want 5323060 (candidate row)", resolved.File.ID)
	}
}

type fakeFileResolverForTransportTest struct {
	file *models.MediaFile
}

func (f fakeFileResolverForTransportTest) GetByID(ctx context.Context, id int) (*models.MediaFile, error) {
	return f.file, nil
}

func TestStartLocalPlaybackTransportPreservesEpisodeURIOverSeriesCatalogFile(t *testing.T) {
	var requestedTargetURI string
	sessionMgr := playback.NewSessionManager(0, 0)
	h := &PlaybackHandler{
		sessionMgr: sessionMgr,
		fileResolver: fakeFileResolverForTransportTest{
			file: &models.MediaFile{
				ID:        5293604,
				FilePath:  "virtual://series/tt11198330",
				ContentID: "series-tvdb-371572",
			},
		},
		VirtualMediaResolver: VirtualMediaResolverFunc(func(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string) (string, error) {
			requestedTargetURI = virtualURI
			return "http://localhost:8080/stream.mp4", nil
		}),
	}

	opts := playback.TranscodeOpts{
		MediaFileID: 5293604,
		InputPath:   "virtual://series/tt11198330/3/2",
		SessionID:   "test-session",
	}

	_, _ = h.startLocalPlaybackTransportOnce(context.Background(), opts)

	if requestedTargetURI != "virtual://series/tt11198330/3/2" {
		t.Fatalf("VirtualMediaResolver received %q, want virtual://series/tt11198330/3/2", requestedTargetURI)
	}
}
