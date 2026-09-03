package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/transcodeproxy"
)

func TestProxyToTranscodeNodeAcknowledgesOnlyFullDownstreamResponse(t *testing.T) {
	const (
		body       = "complete segment"
		generation = "17"
	)
	tests := []struct {
		name        string
		rangeHeader string
		wantStatus  int
		wantAck     int32
	}{
		{name: "ordinary get", wantStatus: http.StatusOK, wantAck: 1},
		{name: "whole-file range", rangeHeader: "bytes=0-", wantStatus: http.StatusPartialContent, wantAck: 1},
		{name: "partial range", rangeHeader: "bytes=1-", wantStatus: http.StatusPartialContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var acknowledgements atomic.Int32
			seenRange := make(chan string, 1)
			node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					if r.Header.Get(transcodeproxy.RequestHeader) != "1" {
						t.Error("segment request omitted proxy marker")
					}
					seenRange <- r.Header.Get("Range")
					w.Header().Set(transcodeproxy.GenerationHeader, generation)
					http.ServeContent(w, r, "seg_00007.ts", time.Time{}, strings.NewReader(body))
				case http.MethodPost:
					if r.Header.Get(transcodeproxy.GenerationHeader) != generation {
						t.Errorf("ack generation = %q, want %q", r.Header.Get(transcodeproxy.GenerationHeader), generation)
					}
					acknowledgements.Add(1)
					w.WriteHeader(http.StatusNoContent)
				default:
					http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
				}
			}))
			defer node.Close()

			handler := &PlaybackHandler{JWTSecret: "proxy-test-secret"}
			req := withPlaybackRouteParam(
				httptest.NewRequest(http.MethodGet, "/playback/transcode/public/segment/seg_00007.ts", nil),
				"session_id",
				"public",
			)
			req.Header.Set("Range", tt.rangeHeader)
			rr := httptest.NewRecorder()
			handler.proxyToTranscodeNode(rr, req, node.URL, "/transcode/remote/segment/seg_00007.ts")

			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %q", rr.Code, tt.wantStatus, rr.Body.String())
			}
			if got := <-seenRange; got != tt.rangeHeader {
				t.Fatalf("forwarded Range = %q, want %q", got, tt.rangeHeader)
			}
			if got := rr.Header().Get(transcodeproxy.GenerationHeader); got != "" {
				t.Fatalf("internal generation leaked downstream: %q", got)
			}
			if got := acknowledgements.Load(); got != tt.wantAck {
				t.Fatalf("acknowledgements = %d, want %d", got, tt.wantAck)
			}
		})
	}
}

func TestProxyToTranscodeNodeDoesNotAcknowledgeFailedDownstreamWrite(t *testing.T) {
	const body = "complete segment"
	var acknowledgements atomic.Int32
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			acknowledgements.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set(transcodeproxy.GenerationHeader, "21")
		http.ServeContent(w, r, "seg_00007.ts", time.Time{}, strings.NewReader(body))
	}))
	defer node.Close()

	handler := &PlaybackHandler{JWTSecret: "proxy-test-secret"}
	req := withPlaybackRouteParam(
		httptest.NewRequest(http.MethodGet, "/playback/transcode/public/segment/seg_00007.ts", nil),
		"session_id",
		"public",
	)
	w := &failingProxyResponseWriter{header: make(http.Header), remaining: 5}
	handler.proxyToTranscodeNode(w, req, node.URL, "/transcode/remote/segment/seg_00007.ts")

	if got := acknowledgements.Load(); got != 0 {
		t.Fatalf("failed downstream transfer produced %d acknowledgement(s)", got)
	}
}

type failingProxyResponseWriter struct {
	header    http.Header
	status    int
	remaining int
}

func (w *failingProxyResponseWriter) Header() http.Header { return w.header }

func (w *failingProxyResponseWriter) WriteHeader(status int) { w.status = status }

func (w *failingProxyResponseWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, io.ErrClosedPipe
	}
	n := min(len(p), w.remaining)
	w.remaining -= n
	return n, io.ErrClosedPipe
}
