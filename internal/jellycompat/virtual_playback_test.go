package jellycompat

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

func TestPrepareVirtualPlaybackVersionBindsProbedCandidate(t *testing.T) {
	file := &models.MediaFile{
		ID: 42, FilePath: "virtual://movie/tt0133093?profile=1080p", Container: "virtual",
		VirtualOwnerInstallationID: 7,
	}
	h := &PlaybackHandler{
		fileResolver: testCompatFileResolver{file: file},
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{
				{URI: "virtual://movie/tt0133093?profile=4K", Label: "4K", OwnerInstallationID: 9},
				{
					URI:                 "virtual://movie/tt0133093?profile=1080p&result=stable",
					Label:               "1080p",
					Container:           "mkv",
					Resolution:          "1080p",
					CodecVideo:          "h264",
					CodecAudio:          "eac3",
					OwnerInstallationID: 11,
				},
			}, nil
		}),
	}
	session := &Session{StreamAppUserID: 23, ProfileID: "viewer"}
	version, uri, owner, err := h.prepareVirtualPlaybackVersion(context.Background(), session, catalog.FileVersion{
		FileID: 42, FilePath: file.FilePath, Container: "virtual",
	})
	if err != nil {
		t.Fatalf("prepareVirtualPlaybackVersion: %v", err)
	}
	if uri != "virtual://movie/tt0133093?profile=1080p&result=stable" || owner != 11 {
		t.Fatalf("bound source = %q owner %d", uri, owner)
	}
	if version.Container != "mkv" || version.CodecAudio != "eac3" || len(version.AudioTracks) != 1 {
		t.Fatalf("probed version = %+v", version)
	}
	if version.FilePath != uri {
		t.Fatalf("version path = %q, want provider-neutral candidate", version.FilePath)
	}
}

func TestShouldUseCompatNodePoolRejectsVirtualSources(t *testing.T) {
	local := PlaybackMediaSource{Version: catalog.FileVersion{FilePath: "/media/movie.mkv", Container: "mkv"}}
	if !shouldUseCompatNodePool(local, &models.MediaFile{FilePath: local.Version.FilePath, Container: local.Version.Container}) {
		t.Fatal("local source should remain eligible for pooled playback")
	}

	virtual := PlaybackMediaSource{
		Version:          catalog.FileVersion{FilePath: "virtual://movie/example", Container: "mkv"},
		VirtualSourceURI: "virtual://movie/example?result=stable",
	}
	if shouldUseCompatNodePool(virtual, &models.MediaFile{FilePath: virtual.Version.FilePath, Container: virtual.Version.Container}) {
		t.Fatal("virtual source must use integrated playback")
	}
}

type virtualBindingSessionManager struct {
	*testCompatSessionManager
	boundURI   string
	boundOwner int
}

func (m *virtualBindingSessionManager) SetVirtualSource(sessionID, uri string, owner int) error {
	m.boundURI, m.boundOwner = uri, owner
	session, err := m.GetSession(sessionID)
	if err != nil {
		return err
	}
	session.VirtualSourceURI = uri
	session.VirtualSourceOwnerInstallationID = owner
	return nil
}

func TestEnsureUpstreamPlaybackBindsVirtualSourceAndReconstructionCard(t *testing.T) {
	manager := &virtualBindingSessionManager{testCompatSessionManager: &testCompatSessionManager{sessions: make(map[string]*playback.Session)}}
	store := NewPlaybackSessionStore(time.Hour, time.Now)
	source := PlaybackMediaSource{
		FileID: 42, Version: catalog.FileVersion{FileID: 42, FilePath: "virtual://movie/tt0133093?result=stable", Container: "mkv"},
		SupportsDirectPlay:               true,
		VirtualSourceURI:                 "virtual://movie/tt0133093?result=stable",
		VirtualSourceOwnerInstallationID: 19,
	}
	store.Put(PlaybackSession{ID: "compat-play", CompatToken: "token", MediaSources: []PlaybackMediaSource{source}})
	h := &PlaybackHandler{sessionMgr: manager, playbackStore: store}
	compatSession := &Session{Token: "token", StreamAppUserID: 7, ProfileID: "profile-a"}
	playSession, err := h.ensureUpstreamPlayback(context.Background(), compatSession, "compat-play", source, "direct")
	if err != nil {
		t.Fatalf("ensureUpstreamPlayback: %v", err)
	}
	if manager.boundURI != source.VirtualSourceURI || manager.boundOwner != 19 {
		t.Fatalf("bound source = %q owner %d", manager.boundURI, manager.boundOwner)
	}
	card := h.upstreamRecipeCard(playSession, compatSession, source, "direct")
	if card.InputPath != source.VirtualSourceURI || card.VirtualSourceOwnerInstallationID != 19 {
		t.Fatalf("reconstruction card = %+v", card)
	}
	reconstructed := playback.NewTranscodeManager()
	reconstructed.Sessions = playback.NewSessionManager(0, 0)
	got := reconstructed.ReconstructSession(context.Background(), playSession.UpstreamSessionID, 7, card)
	if got == nil || got.VirtualSourceURI != source.VirtualSourceURI || got.VirtualSourceOwnerInstallationID != 19 {
		t.Fatalf("reconstructed session = %+v", got)
	}
}

type recordingCompatRelay struct {
	proxiedURL      string
	proxiedInsecure string
	body            string
}

func (r *recordingCompatRelay) Proxy(w http.ResponseWriter, _ *http.Request, source string) error {
	r.proxiedURL = source
	_, _ = io.WriteString(w, r.body)
	return nil
}

func (r *recordingCompatRelay) ProxyInsecure(w http.ResponseWriter, _ *http.Request, source string) error {
	r.proxiedInsecure = source
	_, _ = io.WriteString(w, r.body)
	return nil
}

func (r *recordingCompatRelay) Register(context.Context, string) (string, func(), error) {
	return "http://127.0.0.1/relay", func() {}, nil
}

func TestServeVirtualDirectResolvesBoundSourceThroughRelay(t *testing.T) {
	relay := &recordingCompatRelay{body: "media"}
	var resolvedURI string
	h := &PlaybackHandler{
		RemoteStreamRelay: relay,
		VirtualMediaResolver: VirtualMediaResolverFunc(func(_ context.Context, uri string, owner, user int, profile string) (string, error) {
			resolvedURI = strings.Join([]string{uri, profile}, "|")
			if owner != 3 || user != 8 {
				t.Fatalf("resolver identity owner=%d user=%d", owner, user)
			}
			return "https://provider.example/file", nil
		}),
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Videos/item/stream", nil)
	source := PlaybackMediaSource{VirtualSourceURI: "virtual://movie/tt0133093?result=stable", VirtualSourceOwnerInstallationID: 3}
	if err := h.serveVirtualDirect(w, r, &Session{StreamAppUserID: 8, ProfileID: "kid"}, source); err != nil {
		t.Fatalf("serveVirtualDirect: %v", err)
	}
	if resolvedURI != source.VirtualSourceURI+"|kid" || relay.proxiedURL != "https://provider.example/file" || w.Body.String() != "media" {
		t.Fatalf("resolved=%q proxied=%q body=%q", resolvedURI, relay.proxiedURL, w.Body.String())
	}
}

func TestServeVirtualDirectRoutesInsecureOptInThroughProxyInsecure(t *testing.T) {
	relay := &recordingCompatRelay{body: "media"}
	h := &PlaybackHandler{
		RemoteStreamRelay: relay,
		AllowInsecureVirtual: func(installationID int) bool {
			return installationID == 3
		},
		VirtualMediaResolver: VirtualMediaResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string) (string, error) {
			return "http://10.0.0.7/private", nil
		}),
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Videos/item/stream", nil)
	source := PlaybackMediaSource{VirtualSourceURI: "virtual://movie/tt0133093", VirtualSourceOwnerInstallationID: 3}
	if err := h.serveVirtualDirect(w, r, &Session{StreamAppUserID: 8, ProfileID: "kid"}, source); err != nil {
		t.Fatalf("serveVirtualDirect: %v", err)
	}
	if relay.proxiedInsecure != "http://10.0.0.7/private" {
		t.Fatalf("insecure proxied URL = %q", relay.proxiedInsecure)
	}
}

func TestHandleDownloadReusesBoundVirtualSource(t *testing.T) {
	codec := NewResourceIDCodec()
	contentID := "movie-1"
	placeholderURI := "virtual://movie/tt0133093?profile=1080p"
	boundURI := placeholderURI + "&result=stable"
	version := catalog.FileVersion{FileID: 42, FilePath: placeholderURI, Container: "virtual"}
	boundVersion := version
	boundVersion.FilePath = boundURI
	boundVersion.Container = "mkv"
	boundVersion.CodecVideo = "h264"
	boundVersion.CodecAudio = "aac"
	sourceID := codec.EncodeIntID(EncodedIDMediaSource, 42)
	store := NewPlaybackSessionStore(time.Hour, time.Now)
	store.Put(PlaybackSession{
		ID: "play-1", CompatToken: "token",
		MediaSources: []PlaybackMediaSource{{
			ID: sourceID, FileID: 42, Version: boundVersion,
			VirtualSourceURI: boundURI, VirtualSourceOwnerInstallationID: 7,
		}},
	})
	relay := &recordingCompatRelay{body: "virtual media"}
	resolverCalls := 0
	h := &PlaybackHandler{
		codec:             codec,
		content:           &stubContentService{detail: &upstreamItemDetail{ContentID: contentID, Type: "movie", Versions: []catalog.FileVersion{version}}},
		fileResolver:      testCompatFileResolver{file: &models.MediaFile{ID: 42, FilePath: placeholderURI, Container: "virtual"}},
		playbackStore:     store,
		RemoteStreamRelay: relay,
		VirtualMediaResolver: VirtualMediaResolverFunc(func(_ context.Context, uri string, owner, _ int, _ string) (string, error) {
			resolverCalls++
			if uri != boundURI || owner != 7 {
				t.Fatalf("resolved source = %q owner %d", uri, owner)
			}
			return "https://provider.example/bound", nil
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(context.Context, string, int, string, int) ([]VirtualPlaybackStream, error) {
			t.Fatal("download re-listed provider streams instead of reusing the bound source")
			return nil, nil
		}),
		VirtualSourceProber: func(context.Context, string, *models.MediaFile) (*models.MediaFile, error) {
			t.Fatal("download re-probed the bound source")
			return nil, nil
		},
	}

	encodedID := codec.EncodeStringID(EncodedIDItem, contentID)
	req := httptest.NewRequest(http.MethodGet, "/Items/"+encodedID+"/Download?PlaySessionId=play-1&MediaSourceId="+sourceID, nil)
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("id", encodedID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx)
	ctx = context.WithValue(ctx, compatSessionKey, &Session{Token: "token", StreamAppUserID: 1, ProfileID: "profile-1"})
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.HandleDownload(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "virtual media" {
		t.Fatalf("download response = %d %q", rec.Code, rec.Body.String())
	}
	if resolverCalls != 1 || relay.proxiedURL != "https://provider.example/bound" {
		t.Fatalf("resolver calls=%d proxied=%q", resolverCalls, relay.proxiedURL)
	}
}

func TestMergeCompatCandidateTracks(t *testing.T) {
	file := &models.MediaFile{
		Resolution: "1080p",
		CodecAudio: "eac3",
	}
	candidate := VirtualPlaybackStream{
		Resolution:        "1080p",
		CodecAudio:        "eac3",
		AudioLanguages:    []string{"eng", "jpn"},
		SubtitleLanguages: []string{"eng"},
	}

	mergeCompatCandidateTracks(file, candidate)

	if len(file.AudioTracks) != 2 {
		t.Fatalf("len(AudioTracks) = %d, want 2", len(file.AudioTracks))
	}
	if file.AudioTracks[0].Language != "eng" || !file.AudioTracks[0].Default || file.AudioTracks[0].Channels != 6 {
		t.Fatalf("AudioTrack[0] = %#v", file.AudioTracks[0])
	}
	if file.AudioTracks[1].Language != "jpn" || file.AudioTracks[1].Channels != 6 {
		t.Fatalf("AudioTrack[1] = %#v", file.AudioTracks[1])
	}

	if len(file.SubtitleTracks) != 1 || file.SubtitleTracks[0].Language != "eng" {
		t.Fatalf("SubtitleTracks = %#v", file.SubtitleTracks)
	}
}

func TestMergeCompatCandidateTracksPreservesDVProfile(t *testing.T) {
	file := &models.MediaFile{Resolution: "2160p", CodecVideo: "hevc"}
	mergeCompatCandidateTracks(file, VirtualPlaybackStream{HDR: "Dolby Vision Profile 8.1"})
	if len(file.VideoTracks) != 1 {
		t.Fatalf("video tracks = %#v", file.VideoTracks)
	}
	if file.VideoTracks[0].DVProfile != 8 || file.VideoTracks[0].DolbyVision != "Profile 8" {
		t.Fatalf("DV metadata = %#v", file.VideoTracks[0])
	}
}

func TestMergeCompatCandidateTracksPreservesDVMinorProfileForDetection(t *testing.T) {
	file := &models.MediaFile{Resolution: "2160p", CodecVideo: "hevc"}
	mergeCompatCandidateTracks(file, VirtualPlaybackStream{HDR: "Dolby Vision Profile 8.1"})
	if file.VideoTracks[0].VideoRange != "DolbyVision" || file.VideoTracks[0].DVProfile != 8 {
		t.Fatalf("DV profile detection = %#v", file.VideoTracks[0])
	}
}

func TestCompatVirtualDVProfileWithoutExplicitMarkerIsNotInferred(t *testing.T) {
	file := &models.MediaFile{Resolution: "1080p", CodecVideo: "hevc"}
	mergeCompatCandidateTracks(file, VirtualPlaybackStream{HDR: "true"})
	if len(file.VideoTracks) != 1 || file.VideoTracks[0].DVProfile != 0 {
		t.Fatalf("generic HDR must not become DV: %#v", file.VideoTracks)
	}
}

func TestMergeCompatCandidateTracksRepairsStaleSDRRange(t *testing.T) {
	file := &models.MediaFile{
		HDR:        true,
		Resolution: "2160p",
		CodecVideo: "hevc",
		VideoTracks: []models.VideoTrack{{
			VideoRange:     "SDR",
			VideoRangeType: "SDR",
		}},
	}

	mergeCompatCandidateTracks(file, VirtualPlaybackStream{HDR: "true"})

	if got := file.VideoTracks[0].VideoRange; got != "HDR" {
		t.Fatalf("video range = %q, want HDR", got)
	}
	if got := file.VideoTracks[0].VideoRangeType; got != "HDR10" {
		t.Fatalf("video range type = %q, want HDR10", got)
	}
}

func TestResolveAndProbeVirtualSourcePreservesDVProfileOnReplay(t *testing.T) {
	h := &PlaybackHandler{}
	file := &models.MediaFile{
		ID:       42,
		FilePath: "virtual://movie/tt0133093?profile=1080p&result=stable",
		HDR:      true,
		VideoTracks: []models.VideoTrack{{
			Codec:       "hevc",
			DVProfile:   5,
			DolbyVision: "Profile 5",
			VideoRange:  "DolbyVision",
		}},
	}
	resolved, err := h.resolveAndProbeVirtualSource(context.Background(), file, 1, "profile-1")
	if err != nil {
		t.Fatalf("resolveAndProbeVirtualSource: %v", err)
	}
	if len(resolved.file.VideoTracks) != 1 || resolved.file.VideoTracks[0].DVProfile != 5 {
		t.Fatalf("replay video tracks = %#v, want DVProfile=5 preserved", resolved.file.VideoTracks)
	}
	if resolved.file.VideoTracks[0].VideoRange != "DolbyVision" {
		t.Fatalf("replay VideoRange = %q, want DolbyVision", resolved.file.VideoTracks[0].VideoRange)
	}
}

func TestCompatVirtualDVProfileRobustExtraction(t *testing.T) {
	cases := []struct {
		raw         string
		wantProfile int
	}{
		{raw: "Dolby Vision Profile 8.1", wantProfile: 8},
		{raw: "Dolby Vision Profile 5", wantProfile: 5},
		{raw: "Dolby Vision Profile 7", wantProfile: 7},
		{raw: "dv5", wantProfile: 5},
		{raw: "dv 7", wantProfile: 7},
		{raw: "dovi 08.06", wantProfile: 8},
		{raw: "Dolby Vision 5", wantProfile: 5},
		{raw: "4K Dolby Vision", wantProfile: 0},
		{raw: "Dolby Vision 4K", wantProfile: 0},
		{raw: "Dolby Vision 2160p", wantProfile: 0},
		{raw: "DV 1080p", wantProfile: 0},
		{raw: "Dolby Vision 10bit", wantProfile: 0},
		{raw: "HDR10", wantProfile: 0},
		{raw: "DVD", wantProfile: 0},
	}
	for _, tc := range cases {
		gotProf := compatVirtualDVProfile(tc.raw)
		if gotProf != tc.wantProfile {
			t.Errorf("compatVirtualDVProfile(%q) = %d, want %d", tc.raw, gotProf, tc.wantProfile)
		}
	}
}
