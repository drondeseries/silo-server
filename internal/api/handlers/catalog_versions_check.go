package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
)

const (
	// maxVersionCheckFiles caps the batch so a single request cannot fan out
	// unbounded provider work.
	maxVersionCheckFiles = 40
	// versionCheckConcurrency bounds simultaneous provider resolutions.
	versionCheckConcurrency = 4
	// versionCheckPerFileBudget bounds one file's provider resolution.
	versionCheckPerFileBudget = 6 * time.Second
	// versionCheckOverallBudget bounds the whole batch.
	versionCheckOverallBudget = 15 * time.Second
)

type versionCheckRequest struct {
	FileIDs []int `json:"file_ids"`
}

type versionCheckResult struct {
	FileID    int  `json:"file_id"`
	Available bool `json:"available"`
}

type versionCheckResponse struct {
	Results []versionCheckResult `json:"results"`
}

// HandleCheckVersions implements POST /catalog/versions/check: a batched
// liveness probe for the media page's version list. Each file is tested
// cheaply — virtual rows resolve their pinned ?result= candidate through the
// provider (no media transfer), local rows are read from missing_since — and
// the durable failed_at signal is stamped accordingly. Unknown or deleted
// file IDs are reported as available=false.
func (h *CatalogResourceHandler) HandleCheckVersions(w http.ResponseWriter, r *http.Request) {
	var req versionCheckRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.FileIDs) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "At least one file ID is required")
		return
	}
	if len(req.FileIDs) > maxVersionCheckFiles {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "At most 40 file IDs are allowed per request")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), versionCheckOverallBudget)
	defer cancel()

	results := make([]versionCheckResult, 0, len(req.FileIDs))
	var mu sync.Mutex
	eg, egCtx := errgroup.WithContext(ctx)
	eg.SetLimit(versionCheckConcurrency)
	for _, id := range req.FileIDs {
		if id <= 0 {
			continue
		}
		id := id
		eg.Go(func() error {
			available := h.checkVersion(egCtx, id)
			mu.Lock()
			results = append(results, versionCheckResult{FileID: id, Available: available})
			mu.Unlock()
			return nil
		})
	}
	// Per-file failures are folded into the availability verdict; the batch
	// itself never fails on one file.
	_ = eg.Wait()

	writeJSON(w, http.StatusOK, versionCheckResponse{Results: results})
}

// checkVersion tests one media file's liveness and returns whether it is
// available. Virtual rows resolve the pinned candidate through the provider
// and stamp failed_at on a confirmed dead pin; local rows are read from
// missing_since with no probe. Ambiguous provider errors (timeout, network,
// resolver not configured) leave the stamp unchanged and report the row's
// current computed availability, so a provider outage cannot mass-tag
// versions as dead.
func (h *CatalogResourceHandler) checkVersion(ctx context.Context, fileID int) bool {
	if h == nil || h.FileResolver == nil {
		return false
	}
	file, err := h.FileResolver.GetByID(ctx, fileID)
	if err != nil || file == nil {
		return false
	}
	if !isVirtualPlaybackFile(file) {
		return file.MissingSince == nil
	}
	if h.VirtualResolver == nil {
		// No provider resolver wired (playback disabled): report the durable
		// stamp only, never stamp anything.
		return file.FailedAt == nil
	}

	perFileCtx, cancel := context.WithTimeout(ctx, versionCheckPerFileBudget)
	defer cancel()
	_, err = h.VirtualResolver.ResolveVirtualMediaDetailed(
		perFileCtx, file.FilePath, file.VirtualOwnerInstallationID,
		apimw.GetUserID(ctx), apimw.GetProfileID(ctx), false, nil, "",
	)
	if err == nil {
		// The pinned candidate resolved: it is live. Clear any stale failed
		// stamp so the auto-pick considers it again.
		if h.ClearVirtualFailed != nil {
			_ = h.ClearVirtualFailed(context.WithoutCancel(ctx), fileID)
		}
		return true
	}
	if isVirtualCandidateDeadError(err) {
		// Confirmed dead pin: the provider listed but the pinned candidate is
		// gone or unusable. Stamp it so the auto-pick skips it.
		if h.MarkVirtualFailed != nil {
			_ = h.MarkVirtualFailed(context.WithoutCancel(ctx), fileID)
		}
		return false
	}
	// Ambiguous (provider down, timeout): do not stamp. Report the current
	// durable signal so an outage does not mass-tag versions.
	return file.FailedAt == nil
}

// isVirtualCandidateDeadError classifies a resolution failure as a confirmed
// dead pin: the provider answered but the pinned candidate is no longer
// offered or usable. Provider-down/timeout errors do not match and are treated
// as ambiguous. The strings are the provider-neutral error texts the plugin
// service produces (see internal/plugins/virtual_playback.go). A joined error
// that also carries a provider RPC failure ("request failed") is ambiguous even
// when a fallback provider reported no matching candidate: the owner provider
// that owns the pin may simply be down.
func isVirtualCandidateDeadError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "request failed") ||
		strings.Contains(msg, "resolver is not installed") ||
		strings.Contains(msg, "load owning virtual stream provider") {
		return false
	}
	return strings.Contains(msg, "no matching candidate") ||
		strings.Contains(msg, "no streams available") ||
		strings.Contains(msg, "no usable stream")
}
