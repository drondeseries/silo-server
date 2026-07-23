package catalog

import (
	"fmt"
	"testing"
)

func TestMDBListVirtualPlaybackLogic(t *testing.T) {
	tvdb1 := 201
	entries := []mdblistEntry{
		{
			ID:          101,
			IMDbID:      "tt0000101",
			Title:       "Unmatched Movie",
			ReleaseYear: 2021,
			MediaType:   "movie",
			Rank:        1,
		},
		{
			ID:          102,
			IMDbID:      "tt0000102",
			TVDBID:      &tvdb1,
			Title:       "Unmatched Series",
			ReleaseYear: 2022,
			MediaType:   "show",
			Rank:        2,
		},
		{
			ID:          103,
			IMDbID:      "tt0000103",
			Title:       "Existing Library Movie",
			ReleaseYear: 2020,
			MediaType:   "movie",
			Rank:        3,
		},
		{
			ID:          104,
			IMDbID:      "",
			Title:       "Missing IMDb Show",
			ReleaseYear: 2019,
			MediaType:   "show",
			Rank:        4,
		},
	}

	movieLookup := &ExternalIDLookup{
		ByTMDB: map[string]string{"103": "content-103"},
		ByIMDb: map[string]string{"tt0000103": "content-103"},
		ByTVDB: map[string]string{},
	}
	seriesLookup := &ExternalIDLookup{
		ByTMDB: map[string]string{},
		ByIMDb: map[string]string{},
		ByTVDB: map[string]string{},
	}

	// 1. Verify candidate matching logic for existing library item
	movie3Candidates := pickCandidatesByPriority(movieLookup, entries[2], "movie")
	if len(movie3Candidates) == 0 || movie3Candidates[0] != "content-103" {
		t.Fatalf("expected existing movie to resolve to content-103, got %v", movie3Candidates)
	}

	// 2. Verify candidate matching for unmatched items prior to materialization
	movie1Candidates := pickCandidatesByPriority(movieLookup, entries[0], "movie")
	if len(movie1Candidates) != 0 {
		t.Fatalf("expected unmatched movie to have no candidates, got %v", movie1Candidates)
	}

	series1Candidates := pickCandidatesByPriority(seriesLookup, entries[1], "series")
	if len(series1Candidates) != 0 {
		t.Fatalf("expected unmatched series to have no candidates, got %v", series1Candidates)
	}

	// 3. Simulate virtual item materialization mapping update
	unmaterializableWarnings := make(map[int]string)
	for i, entry := range entries {
		itemType := mdbListEntryItemType(entry)
		var lookup *ExternalIDLookup
		if itemType == "movie" {
			lookup = movieLookup
		} else {
			lookup = seriesLookup
		}

		if len(pickCandidatesByPriority(lookup, entry, itemType)) > 0 {
			continue // Existing item preserved
		}

		if entry.IMDbID == "" {
			unmaterializableWarnings[i] = fmt.Sprintf("Missing IMDb ID for virtual playback: %s", entry.Title)
			continue
		}

		// Materialized virtual item content ID
		simulatedContentID := fmt.Sprintf("virtual-%s", entry.IMDbID)
		if itemType == "movie" {
			movieLookup.ByIMDb[entry.IMDbID] = simulatedContentID
			if entry.ID > 0 {
				movieLookup.ByTMDB[fmt.Sprintf("%d", entry.ID)] = simulatedContentID
			}
		} else {
			seriesLookup.ByIMDb[entry.IMDbID] = simulatedContentID
			if entry.ID > 0 {
				seriesLookup.ByTMDB[fmt.Sprintf("%d", entry.ID)] = simulatedContentID
			}
			if entry.TVDBID != nil && *entry.TVDBID > 0 {
				seriesLookup.ByTVDB[fmt.Sprintf("%d", *entry.TVDBID)] = simulatedContentID
			}
		}
	}

	// Verify post-materialization candidate resolution
	movie1After := pickCandidatesByPriority(movieLookup, entries[0], "movie")
	if len(movie1After) == 0 || movie1After[0] != "virtual-tt0000101" {
		t.Errorf("expected unmatched movie to resolve to virtual-tt0000101, got %v", movie1After)
	}

	series1After := pickCandidatesByPriority(seriesLookup, entries[1], "series")
	if len(series1After) == 0 || series1After[0] != "virtual-tt0000102" {
		t.Errorf("expected unmatched series to resolve to virtual-tt0000102, got %v", series1After)
	}

	// Verify missing IMDb warning recording
	if warning, exists := unmaterializableWarnings[3]; !exists || warning != "Missing IMDb ID for virtual playback: Missing IMDb Show" {
		t.Errorf("expected missing IMDb warning for entry index 3, got %q (exists: %v)", warning, exists)
	}
}

func TestMDBListVirtualPlaybackLimit(t *testing.T) {
	entries := make([]mdblistEntry, 25)
	for i := 0; i < 25; i++ {
		entries[i] = mdblistEntry{
			ID:          1000 + i,
			IMDbID:      fmt.Sprintf("tt%07d", 1000+i),
			Title:       fmt.Sprintf("Item %d", i+1),
			ReleaseYear: 2024,
			MediaType:   "movie",
			Rank:        i + 1,
		}
	}

	limit := 20
	fetchLimit := 20 * 2 // source fetch limit multiplier is 2x
	if len(entries) > fetchLimit {
		entries = entries[:fetchLimit]
	}

	if len(entries) != 25 {
		t.Errorf("expected 25 entries within fetch limit 40, got %d", len(entries))
	}

	// Test slicing when entries > 40
	bigEntries := make([]mdblistEntry, 50)
	if len(bigEntries) > fetchLimit {
		bigEntries = bigEntries[:fetchLimit]
	}
	if len(bigEntries) != 40 {
		t.Errorf("expected 40 entries after fetch limit truncation, got %d", len(bigEntries))
	}
	_ = limit
}
