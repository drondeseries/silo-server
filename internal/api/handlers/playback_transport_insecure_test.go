package handlers

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/remotestream"
)

func TestResolveVirtualInputRelayRespectsAllowInsecureOptIn(t *testing.T) {
	relay := remotestream.NewRelay()
	defer func() { _ = relay.Close(context.Background()) }()

	h := &PlaybackHandler{
		RemoteStreamRelay: relay,
		VirtualMediaResolver: VirtualMediaResolverFunc(func(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string) (string, error) {
			return "http://altmount:8080/stremio/test/play?url=http%3A%2F%2Fprowlarr%3A9696%2F19%2Fdownload", nil
		}),
	}

	t.Run("opt-in off rejects private host", func(t *testing.T) {
		h.AllowInsecureVirtual = nil
		relayURL, cleanup, err := h.resolveVirtualInputURI(context.Background(), "virtual://series/tt1/1/1", 5, 1, "profile", false)
		if err == nil {
			cleanup()
			t.Fatalf("expected private host to be rejected without allow_insecure_http, got relay %q", relayURL)
		}
	})

	t.Run("opt-in on accepts private host", func(t *testing.T) {
		h.AllowInsecureVirtual = func(installationID int) bool { return installationID == 5 }
		relayURL, cleanup, err := h.resolveVirtualInputURI(context.Background(), "virtual://series/tt1/1/1", 5, 1, "profile", false)
		if err != nil {
			t.Fatalf("expected private host to be accepted with allow_insecure_http: %v", err)
		}
		defer cleanup()
		if relayURL == "" {
			t.Fatal("expected a relay URL")
		}
	})

	t.Run("opt-in on for other installation stays strict", func(t *testing.T) {
		h.AllowInsecureVirtual = func(installationID int) bool { return installationID == 5 }
		if _, cleanup, err := h.resolveVirtualInputURI(context.Background(), "virtual://series/tt1/1/1", 7, 1, "profile", false); err == nil {
			cleanup()
			t.Fatal("expected private host to be rejected for an installation without the opt-in")
		}
	})
}

func TestVirtualTranscodeStartupFailsOverOnResolutionError(t *testing.T) {
	attempts := make([]string, 0)
	h := &PlaybackHandler{
		VirtualMediaResolver: VirtualMediaResolverFunc(func(ctx context.Context, virtualURI string, ownerInstallationID int, userID int, profileID string) (string, error) {
			attempts = append(attempts, virtualURI)
			if virtualURI == "virtual://series/tt1/1/1?result=dead" {
				return "", context.DeadlineExceeded
			}
			return "http://localhost:8080/stream.m3u8", nil
		}),
	}

	// Attempt 0: "virtual://series/tt1/1/1?result=dead"
	relayURL0, _, err0 := h.resolveVirtualInputURI(context.Background(), "virtual://series/tt1/1/1?result=dead", 5, 1, "profile", false)
	if err0 == nil {
		t.Fatalf("expected attempt 0 to fail, got %q", relayURL0)
	}

	// Attempt 1: neutral fallback "virtual://series/tt1/1/1"
	relayURL1, _, err1 := h.resolveVirtualInputURI(context.Background(), "virtual://series/tt1/1/1", 5, 1, "profile", true)
	if err1 != nil {
		t.Fatalf("expected attempt 1 to succeed, got error: %v", err1)
	}
	if relayURL1 != "http://localhost:8080/stream.m3u8" {
		t.Fatalf("expected stream url, got %q", relayURL1)
	}
}
