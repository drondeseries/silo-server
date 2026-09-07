package notifications

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/oklog/ulid/v2"
)

// RatingFlags is the decoded reason_flags shape for rating.set deliveries.
type RatingFlags struct {
	Rating int `json:"rating"`
}

// parseRatingFlags decodes a rating.set delivery's reason_flags.
func parseRatingFlags(raw []byte) RatingFlags {
	var flags RatingFlags
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &flags)
	}
	return flags
}

// RatingNotifier adapts the notification system to the ratings API: when a
// profile rates an item, it posts one durable rating.set delivery gated on
// the profile's master toggle. Webhooks whose notify_ratings flag is set
// receive the payload; web push and Apple push receive it too when enabled.
type RatingNotifier struct {
	system      *System
	itemRepo    *catalog.ItemRepository
	episodeRepo *catalog.EpisodeRepository
	seasonRepo  *catalog.SeasonRepository
}

// NewRatingNotifier creates the adapter. Returns nil when there is no
// notification system. itemRepo is required; episode/season repos are used
// to resolve episodes/seasons up to their parent series.
func NewRatingNotifier(
	system *System,
	itemRepo *catalog.ItemRepository,
	episodeRepo *catalog.EpisodeRepository,
	seasonRepo *catalog.SeasonRepository,
) *RatingNotifier {
	if system == nil || itemRepo == nil {
		return nil
	}
	return &RatingNotifier{
		system:      system,
		itemRepo:    itemRepo,
		episodeRepo: episodeRepo,
		seasonRepo:  seasonRepo,
	}
}

// NotifyRating posts one rating.set delivery for a rated item. contentID is
// the rated media item (movie, series, season, or episode); the rated item's
// parent series is resolved so the delivery row can join catalog metadata for
// outbound rendering. Best-effort and non-blocking: a delivery failure must
// never fail the rating write.
func (n *RatingNotifier) NotifyRating(ctx context.Context, userID int, profileID, contentID string, rating int) error {
	if n == nil || n.system == nil || contentID == "" {
		return nil
	}
	if userID <= 0 || profileID == "" {
		return nil
	}
	prefs, err := n.system.Preferences.Get(ctx, profileID)
	if err != nil {
		return err
	}
	if !prefs.Enabled {
		return nil
	}

	seriesID, episodeID := n.resolveParent(ctx, contentID)

	flags, err := json.Marshal(RatingFlags{Rating: rating})
	if err != nil {
		return fmt.Errorf("marshal rating flags: %w", err)
	}
	delivery := Delivery{
		ID:          ulid.Make().String(),
		UserID:      userID,
		ProfileID:   profileID,
		SeriesID:    seriesID,
		EpisodeID:   episodeID,
		Type:        DeliveryTypeRatingSet,
		ReasonFlags: flags,
	}
	_, err = n.system.DispatchOperational(ctx, delivery, OperationalDispatch{
		WebhookFilter: func(hook Webhook) bool { return hook.NotifyRatings },
	})
	return err
}

// resolveParent maps a rated content ID to its parent series (and itself when
// the rated item is an episode). contentID may be a movie, series, season, or
// episode:
//   - movie / series → seriesID = contentID
//   - season → seriesID = parent series
//   - episode → seriesID = parent series, episodeID = contentID
func (n *RatingNotifier) resolveParent(ctx context.Context, contentID string) (seriesID, episodeID *string) {
	if n.episodeRepo != nil {
		if ep, err := n.episodeRepo.GetByID(ctx, contentID); err == nil && ep != nil {
			sid := ep.SeriesID
			eid := ep.ContentID
			return &sid, &eid
		}
	}
	if n.seasonRepo != nil {
		if season, err := n.seasonRepo.GetByID(ctx, contentID); err == nil && season != nil {
			sid := season.SeriesID
			return &sid, nil
		}
	}
	if item, err := n.itemRepo.GetByID(ctx, contentID); err == nil && item != nil {
		sid := item.ContentID
		return &sid, nil
	}
	return nil, nil
}
