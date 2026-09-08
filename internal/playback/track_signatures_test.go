package playback

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

func TestAudioTrackSignatureLanguagesRoundTrip(t *testing.T) {
	track := models.AudioTrack{
		Language:  "en",
		Title:     "MULTi",
		Codec:     "eac3",
		Layout:    "5.1",
		Channels:  6,
		Languages: []string{"de", "en", "fr"},
	}

	sig := AudioTrackSignatureFromTrack(track)
	if sig == nil {
		t.Fatal("expected non-nil signature")
	}
	// Languages are stored sorted and canonicalized, not in container order.
	want := []string{"de", "en", "fr"}
	if len(sig.Languages) != len(want) {
		t.Fatalf("Languages = %v, want %v", sig.Languages, want)
	}
	for i := range want {
		if sig.Languages[i] != want[i] {
			t.Fatalf("Languages = %v, want %v", sig.Languages, want)
		}
	}

	// The signature survives a JSON marshal/unmarshal round-trip (the persisted
	// series-preference path).
	data, err := userstore.MarshalAudioTrackSignature(sig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	restored, err := userstore.UnmarshalAudioTrackSignature(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if restored == nil {
		t.Fatal("round-tripped signature is nil")
	}
	if len(restored.Languages) != 3 || restored.Languages[0] != "de" {
		t.Fatalf("round-tripped Languages = %v, want [de en fr]", restored.Languages)
	}
}

func TestAudioTrackSignatureMatchesMultisetRegardlessOfOrder(t *testing.T) {
	sig := AudioTrackSignatureFromTrack(models.AudioTrack{
		Language:  "en",
		Codec:     "eac3",
		Channels:  6,
		Languages: []string{"en", "fr"},
	})
	if sig == nil {
		t.Fatal("expected non-nil signature")
	}

	// Same multiset, different container order still matches.
	reordered := models.AudioTrack{
		Language:  "en",
		Codec:     "eac3",
		Channels:  6,
		Languages: []string{"fr", "en"},
	}
	if !audioTrackMatchesSignature(reordered, sig) {
		t.Fatal("reordered MULTi list did not match signature")
	}

	// Same multiset, mixed code spellings ("eng" vs "en") still matches.
	spelled := models.AudioTrack{
		Language:  "eng",
		Codec:     "eac3",
		Channels:  6,
		Languages: []string{"eng", "fra"},
	}
	if !audioTrackMatchesSignature(spelled, sig) {
		t.Fatal("canonicalized MULTi list did not match signature")
	}
}

func TestAudioTrackSignatureMultisetMismatch(t *testing.T) {
	sig := AudioTrackSignatureFromTrack(models.AudioTrack{
		Language:  "en",
		Codec:     "eac3",
		Channels:  6,
		Languages: []string{"en", "fr"},
	})

	// A language added to the track breaks the signature.
	added := models.AudioTrack{
		Language:  "en",
		Codec:     "eac3",
		Channels:  6,
		Languages: []string{"en", "fr", "de"},
	}
	if audioTrackMatchesSignature(added, sig) {
		t.Fatal("added language still matched signature")
	}

	// A language removed from the track breaks the signature.
	removed := models.AudioTrack{
		Language:  "en",
		Codec:     "eac3",
		Channels:  6,
		Languages: []string{"en"},
	}
	if audioTrackMatchesSignature(removed, sig) {
		t.Fatal("removed language still matched signature")
	}
}

func TestLegacySignatureWithoutLanguagesMatchesSingleLanguageTrack(t *testing.T) {
	// Old persisted signatures predate the Languages field and unmarshal to nil.
	legacy := &userstore.AudioTrackSignature{
		Language: "en",
		Codec:    "aac",
		Channels: 2,
	}
	track := models.AudioTrack{
		Language: "en",
		Codec:    "aac",
		Channels: 2,
	}
	if !audioTrackMatchesSignature(track, legacy) {
		t.Fatal("legacy signature without languages did not match single-language track")
	}
	if legacy.IsZero() {
		t.Fatal("legacy signature with a language must not read as zero")
	}
}
