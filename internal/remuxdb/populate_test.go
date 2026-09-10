package remuxdb

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestApplyEvidenceFillsMissingOnly(t *testing.T) {
	ev := Evidence{
		Container:  "mkv",
		CodecVideo: "hevc",
		Resolution: "2160p",
		HDR:        true,
		HDRKnown:   true,
		Duration:   6583.0,
		Bitrate:    58828983,
		VideoTracks: []TrackDetail{
			{Kind: "video", Codec: "hevc", Width: 3840, Height: 2160, DVProfile: 8, ColorTransfer: "smpte2084"},
		},
		AudioTracks: []TrackDetail{
			{Kind: "audio", Codec: "truehd", Channels: 8, Language: "eng"},
		},
	}
	file := &models.MediaFile{ContentID: "movie-1", Resolution: "2160p"}
	if !ApplyEvidence(ev, file) {
		t.Fatal("evidence not applied")
	}
	if file.CodecVideo != "hevc" || !file.HDR || file.CodecAudio != "truehd" || file.AudioChannels != 8 {
		t.Fatalf("file = %+v", file)
	}
	if file.Resolution != "2160p" {
		t.Fatalf("resolution overwritten: %q", file.Resolution)
	}
	if len(file.VideoTracks) != 1 || file.VideoTracks[0].DVProfile != 8 {
		t.Fatalf("video tracks = %+v", file.VideoTracks)
	}
}

func TestApplyEvidenceNeverOverwritesProbeDetail(t *testing.T) {
	ev := Evidence{
		CodecVideo:  "hevc",
		VideoTracks: []TrackDetail{{Kind: "video", Codec: "hevc", Height: 2160}},
		AudioTracks: []TrackDetail{{Kind: "audio", Codec: "truehd", Channels: 8}},
	}
	file := &models.MediaFile{
		CodecVideo:  "h264",
		VideoTracks: []models.VideoTrack{{Codec: "h264", Height: 1080}},
		AudioTracks: []models.AudioTrack{{Codec: "aac", Channels: 2}},
		CodecAudio:  "aac",
		Resolution:  "1080p",
	}
	if !ApplyEvidence(ev, file) {
		t.Fatal("evidence not applied")
	}
	if file.CodecVideo != "h264" || file.Resolution != "1080p" || file.CodecAudio != "aac" {
		t.Fatalf("probe detail overwritten: %+v", file)
	}
	if len(file.VideoTracks) != 1 || file.VideoTracks[0].Codec != "h264" {
		t.Fatalf("probe tracks overwritten: %+v", file.VideoTracks)
	}
	if len(file.AudioTracks) != 1 || file.AudioTracks[0].Codec != "aac" {
		t.Fatalf("probe audio overwritten: %+v", file.AudioTracks)
	}
}

func TestApplyEvidenceRequiresVideo(t *testing.T) {
	file := &models.MediaFile{}
	if ApplyEvidence(Evidence{AudioTracks: []TrackDetail{{Kind: "audio"}}}, file) {
		t.Fatal("audio-only evidence applied")
	}
	if ApplyEvidence(Evidence{}, nil) {
		t.Fatal("nil file applied")
	}
}
