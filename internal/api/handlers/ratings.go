package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/catalog"
)

// ratingsRepository defines the data access interface for user ratings.
type ratingsRepository interface {
	Set(ctx context.Context, userID int, profileID, mediaItemID string, rating int) error
	Get(ctx context.Context, userID int, profileID, mediaItemID string) (*catalog.UserRating, error)
	Delete(ctx context.Context, userID int, profileID, mediaItemID string) error
	List(ctx context.Context, userID int, profileID string, limit, offset int) ([]catalog.UserRating, error)
}

// ratingNotifier is the optional outbound hook fired after a rating is set.
// Implemented by notifications.RatingNotifier; nil when notifications are
// unavailable.
type ratingNotifier interface {
	NotifyRating(ctx context.Context, userID int, profileID, contentID string, rating int) error
}

// RatingsHandler handles user rating operations.
type RatingsHandler struct {
	ratingsRepo             ratingsRepository
	itemRepo                personalDataItemRepository
	profileStaler           ProfileStaler
	profileRefreshRequester ProfileRefreshRequester
	notifier                ratingNotifier
}

// NewRatingsHandler creates a new RatingsHandler.
func NewRatingsHandler(ratingsRepo ratingsRepository, itemRepo personalDataItemRepository) *RatingsHandler {
	return &RatingsHandler{ratingsRepo: ratingsRepo, itemRepo: itemRepo}
}

// SetRatingNotifier configures the optional outbound notification hook.
func (h *RatingsHandler) SetRatingNotifier(n ratingNotifier) {
	h.notifier = n
}

// SetProfileStaler configures an optional staleness trigger for taste profiles.
func (h *RatingsHandler) SetProfileStaler(ps ProfileStaler) {
	h.profileStaler = ps
}

// SetProfileRefreshRequester configures an optional background refresh queue for taste profiles.
func (h *RatingsHandler) SetProfileRefreshRequester(requester ProfileRefreshRequester) {
	h.profileRefreshRequester = requester
}

func (h *RatingsHandler) markStale(ctx context.Context, userID int, profileID string) {
	triggerProfileRefresh(ctx, h.profileStaler, h.profileRefreshRequester, userID, profileID)
}

// --- Response types ---

type ratingResponse struct {
	Rating  int    `json:"rating"`
	RatedAt string `json:"rated_at"`
}

type ratingListItem struct {
	MediaItemID string `json:"media_item_id"`
	Rating      int    `json:"rating"`
	RatedAt     string `json:"rated_at"`
}

type ratingListResponse struct {
	Ratings []ratingListItem `json:"ratings"`
}

// --- Request types ---

type setRatingRequest struct {
	Rating int `json:"rating"`
}

// HandleSetRating handles PUT /ratings/{item_id}.
// Accepts {"rating": N} where N is 1-5. Returns 204 on success.
func (h *RatingsHandler) HandleSetRating(w http.ResponseWriter, r *http.Request) {
	userID := apimw.GetUserID(r.Context())
	profileID := apimw.GetProfileID(r.Context())
	itemID := chi.URLParam(r, "item_id")

	if itemID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Item ID is required")
		return
	}

	var req setRatingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	if req.Rating < 1 || req.Rating > 5 {
		writeError(w, http.StatusBadRequest, "bad_request", "Rating must be between 1 and 5")
		return
	}

	// EnsureAccessible validates the rated id against media_items, so an
	// episode or season id passes only if a media_item with that content_id
	// exists. That is intentional: the handler rejects inaccessible content,
	// while the notifier's resolveParent separately resolves episodes and
	// seasons from their own tables (episodes, seasons) to their parent
	// series. The two code paths deliberately use different entity scopes —
	// access control is item-scoped, parent resolution is media-scoped.
	if err := h.itemRepo.EnsureAccessible(r.Context(), itemID, requestAccessFilter(r)); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}

	if err := h.ratingsRepo.Set(r.Context(), userID, profileID, itemID, req.Rating); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to set rating")
		return
	}

	h.markStale(r.Context(), userID, profileID)
	if h.notifier != nil {
		// Best-effort: a delivery failure must never fail the rating write.
		// Detached from the request context so a client disconnect after the
		// rating write doesn't abort the webhook delivery.
		go func() {
			dispatchCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
			defer cancel()
			if err := h.notifier.NotifyRating(dispatchCtx, userID, profileID, itemID, req.Rating); err != nil {
				slog.WarnContext(context.Background(), "failed to dispatch rating notification",
					"component", "ratings", "item_id", itemID, "error", err)
			}
		}()
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleDeleteRating handles DELETE /ratings/{item_id}.
// Returns 204 on success.
func (h *RatingsHandler) HandleDeleteRating(w http.ResponseWriter, r *http.Request) {
	userID := apimw.GetUserID(r.Context())
	profileID := apimw.GetProfileID(r.Context())
	itemID := chi.URLParam(r, "item_id")

	if itemID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Item ID is required")
		return
	}

	if err := h.ratingsRepo.Delete(r.Context(), userID, profileID, itemID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to delete rating")
		return
	}

	h.markStale(r.Context(), userID, profileID)
	w.WriteHeader(http.StatusNoContent)
}

// HandleGetRating handles GET /ratings/{item_id}.
// Returns the rating or 404 if not found.
func (h *RatingsHandler) HandleGetRating(w http.ResponseWriter, r *http.Request) {
	userID := apimw.GetUserID(r.Context())
	profileID := apimw.GetProfileID(r.Context())
	itemID := chi.URLParam(r, "item_id")

	if itemID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Item ID is required")
		return
	}

	rating, err := h.ratingsRepo.Get(r.Context(), userID, profileID, itemID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to get rating")
		return
	}

	if rating == nil {
		writeError(w, http.StatusNotFound, "not_found", "Rating not found")
		return
	}

	writeJSON(w, http.StatusOK, ratingResponse{
		Rating:  rating.Rating,
		RatedAt: rating.RatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	})
}

// HandleListRatings handles GET /ratings/.
// Returns paginated ratings for the current user+profile.
func (h *RatingsHandler) HandleListRatings(w http.ResponseWriter, r *http.Request) {
	userID := apimw.GetUserID(r.Context())
	profileID := apimw.GetProfileID(r.Context())

	limit, offset := parsePagination(r)

	ratings, err := h.ratingsRepo.List(r.Context(), userID, profileID, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to list ratings")
		return
	}

	items := make([]ratingListItem, 0, len(ratings))
	for _, ur := range ratings {
		items = append(items, ratingListItem{
			MediaItemID: ur.MediaItemID,
			Rating:      ur.Rating,
			RatedAt:     ur.RatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}

	writeJSON(w, http.StatusOK, ratingListResponse{Ratings: items})
}
