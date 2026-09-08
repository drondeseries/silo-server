package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

// --- Stub repos for resolveParent ---

type stubEpisodeLookuper struct {
	ep  *models.Episode
	err error
}

func (s *stubEpisodeLookuper) GetByID(_ context.Context, _ string) (*models.Episode, error) {
	return s.ep, s.err
}

type stubSeasonLookuper struct {
	season *models.Season
	err    error
}

func (s *stubSeasonLookuper) GetByID(_ context.Context, _ string) (*models.Season, error) {
	return s.season, s.err
}

type stubItemLookuper struct {
	item *models.MediaItem
	err  error
}

func (s *stubItemLookuper) GetByID(_ context.Context, _ string) (*models.MediaItem, error) {
	return s.item, s.err
}

// --- parseRatingFlags tests ---

func TestParseRatingFlags(t *testing.T) {
	t.Run("valid rating", func(t *testing.T) {
		flags := parseRatingFlags([]byte(`{"rating":4}`))
		if flags.Rating != 4 {
			t.Fatalf("Rating = %d, want 4", flags.Rating)
		}
	})
	t.Run("empty input", func(t *testing.T) {
		flags := parseRatingFlags(nil)
		if flags.Rating != 0 {
			t.Fatalf("Rating = %d, want 0", flags.Rating)
		}
	})
	t.Run("malformed JSON", func(t *testing.T) {
		flags := parseRatingFlags([]byte(`{bad json`))
		if flags.Rating != 0 {
			t.Fatalf("Rating = %d, want 0 for malformed JSON", flags.Rating)
		}
	})
}

// --- resolveParent tests ---

func TestResolveParent(t *testing.T) {
	ctx := context.Background()

	t.Run("episode", func(t *testing.T) {
		n := &RatingNotifier{
			episodeRepo: &stubEpisodeLookuper{
				ep: &models.Episode{ContentID: "episode-1", SeriesID: "series-1"},
			},
		}
		seriesID, episodeID := n.resolveParent(ctx, "episode-1")
		if seriesID == nil || *seriesID != "series-1" {
			t.Fatalf("seriesID = %v, want series-1", seriesID)
		}
		if episodeID == nil || *episodeID != "episode-1" {
			t.Fatalf("episodeID = %v, want episode-1", episodeID)
		}
	})

	t.Run("season", func(t *testing.T) {
		n := &RatingNotifier{
			episodeRepo: &stubEpisodeLookuper{err: errors.New("not found")},
			seasonRepo: &stubSeasonLookuper{
				season: &models.Season{SeriesID: "series-1"},
			},
		}
		seriesID, episodeID := n.resolveParent(ctx, "season-1")
		if seriesID == nil || *seriesID != "series-1" {
			t.Fatalf("seriesID = %v, want series-1", seriesID)
		}
		// C9 fix: season contentID is returned as episodeID so item_id
		// is the season, not the series.
		if episodeID == nil || *episodeID != "season-1" {
			t.Fatalf("episodeID = %v, want season-1 (the rated season)", episodeID)
		}
	})

	t.Run("movie/series", func(t *testing.T) {
		n := &RatingNotifier{
			episodeRepo: &stubEpisodeLookuper{err: errors.New("not found")},
			seasonRepo:  &stubSeasonLookuper{err: errors.New("not found")},
			itemRepo: &stubItemLookuper{
				item: &models.MediaItem{ContentID: "movie-1"},
			},
		}
		seriesID, episodeID := n.resolveParent(ctx, "movie-1")
		if seriesID == nil || *seriesID != "movie-1" {
			t.Fatalf("seriesID = %v, want movie-1", seriesID)
		}
		if episodeID != nil {
			t.Fatalf("episodeID = %v, want nil for movie/series", *episodeID)
		}
	})

	t.Run("unknown contentID falls back to contentID", func(t *testing.T) {
		n := &RatingNotifier{
			episodeRepo: &stubEpisodeLookuper{err: errors.New("not found")},
			seasonRepo:  &stubSeasonLookuper{err: errors.New("not found")},
			itemRepo:    &stubItemLookuper{err: errors.New("not found")},
		}
		seriesID, episodeID := n.resolveParent(ctx, "unknown-1")
		// C3 fix: unknown contentID falls back to using contentID as seriesID
		// so item_id is never empty.
		if seriesID == nil || *seriesID != "unknown-1" {
			t.Fatalf("seriesID = %v, want unknown-1 (C3 fallback)", seriesID)
		}
		if episodeID != nil {
			t.Fatalf("episodeID = %v, want nil for unknown content", *episodeID)
		}
	})
}

// --- BuildGenericWebhookPayload rating.set tests ---

func TestBuildGenericWebhookPayload_RatingSet(t *testing.T) {
	t.Run("episode item_id", func(t *testing.T) {
		epID := "episode-1"
		serID := "series-1"
		flags, _ := json.Marshal(RatingFlags{Rating: 5})
		row := DeliveryRow{
			Delivery: Delivery{
				Type:        DeliveryTypeRatingSet,
				SeriesID:    &serID,
				EpisodeID:   &epID,
				ReasonFlags: flags,
			},
		}
		body, err := BuildGenericWebhookPayload(row, "hook-1", false)
		if err != nil {
			t.Fatalf("BuildGenericWebhookPayload: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		rating, ok := payload["rating"].(map[string]any)
		if !ok {
			t.Fatal("payload missing rating object")
		}
		if rating["item_id"] != "episode-1" {
			t.Fatalf("item_id = %v, want episode-1", rating["item_id"])
		}
		if rating["rating"] != float64(5) {
			t.Fatalf("rating = %v, want 5", rating["rating"])
		}
	})

	t.Run("season item_id", func(t *testing.T) {
		serID := "series-1"
		// Season: episodeID carries the season's contentID (per C9 fix),
		// so item_id is the season, not the series.
		seasonID := "season-1"
		flags, _ := json.Marshal(RatingFlags{Rating: 3})
		row := DeliveryRow{
			Delivery: Delivery{
				Type:        DeliveryTypeRatingSet,
				SeriesID:    &serID,
				EpisodeID:   &seasonID,
				ReasonFlags: flags,
			},
		}
		body, err := BuildGenericWebhookPayload(row, "hook-1", false)
		if err != nil {
			t.Fatalf("BuildGenericWebhookPayload: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		rating, ok := payload["rating"].(map[string]any)
		if !ok {
			t.Fatal("payload missing rating object")
		}
		if rating["item_id"] != "season-1" {
			t.Fatalf("item_id = %v, want season-1", rating["item_id"])
		}
	})

	t.Run("movie item_id", func(t *testing.T) {
		serID := "movie-1"
		flags, _ := json.Marshal(RatingFlags{Rating: 2})
		row := DeliveryRow{
			Delivery: Delivery{
				Type:        DeliveryTypeRatingSet,
				SeriesID:    &serID,
				EpisodeID:   nil,
				ReasonFlags: flags,
			},
		}
		body, err := BuildGenericWebhookPayload(row, "hook-1", false)
		if err != nil {
			t.Fatalf("BuildGenericWebhookPayload: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		rating, ok := payload["rating"].(map[string]any)
		if !ok {
			t.Fatal("payload missing rating object")
		}
		if rating["item_id"] != "movie-1" {
			t.Fatalf("item_id = %v, want movie-1", rating["item_id"])
		}
	})
}

// --- NotifyRating nil/edge tests ---

func TestNotifyRatingNilGuards(t *testing.T) {
	ctx := context.Background()
	if err := (&RatingNotifier{}).NotifyRating(ctx, 1, "p", "c", 5); err != nil {
		t.Fatalf("nil notifier should return nil, got %v", err)
	}
	n := &RatingNotifier{system: &System{}}
	if err := n.NotifyRating(ctx, 0, "p", "c", 5); err != nil {
		t.Fatalf("userID <= 0 should return nil, got %v", err)
	}
	if err := n.NotifyRating(ctx, 1, "", "c", 5); err != nil {
		t.Fatalf("empty profileID should return nil, got %v", err)
	}
	if err := n.NotifyRating(ctx, 1, "p", "", 5); err != nil {
		t.Fatalf("empty contentID should return nil, got %v", err)
	}
}
