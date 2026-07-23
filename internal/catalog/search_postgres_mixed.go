package catalog

import (
	"context"
	"fmt"
	"strings"

	"github.com/Silo-Server/silo-server/internal/access"
	"github.com/Silo-Server/silo-server/internal/models"
)

// GetSearchItemsByIDsWithAccess hydrates mixed search hits in one round trip.
// Meilisearch IDs are only candidates: both branches reapply the effective
// access filter, and callers restore provider order after this method returns.
func (r *ItemRepository) GetSearchItemsByIDsWithAccess(
	ctx context.Context,
	contentIDs []string,
	filter AccessFilter,
) ([]*models.MediaItem, error) {
	if len(contentIDs) == 0 || (filter.AllowedLibraryIDs != nil && len(filter.AllowedLibraryIDs) == 0) {
		return []*models.MediaItem{}, nil
	}

	args := []any{contentIDs}
	argIdx := 2
	mediaConditions := []string{"hydrated_mi.content_id = ANY($1)"}
	appendLibraryAccessConditions("hydrated_mi.content_id", filter, &mediaConditions, &args, &argIdx)
	applyAccessFilter("hydrated_mi", AccessFilter{
		MaxContentRating:   filter.MaxContentRating,
		ExcludedMediaTypes: filter.ExcludedMediaTypes,
	}, &mediaConditions, &args, &argIdx)

	episodeConditions := []string{"mi.content_id = ANY($1)"}
	appendEpisodeLibrarySearchAccess("mi.content_id", filter, &episodeConditions, &args, &argIdx)
	applyAccessFilter("mi", AccessFilter{
		MaxContentRating:   filter.MaxContentRating,
		ExcludedMediaTypes: filter.ExcludedMediaTypes,
	}, &episodeConditions, &args, &argIdx)

	query := fmt.Sprintf(`
		SELECT %s
		FROM media_items hydrated_mi
		WHERE %s
		UNION ALL
		SELECT %s
		FROM %s
		WHERE %s`,
		qualifiedItemColumns("hydrated_mi"), strings.Join(mediaConditions, " AND "),
		qualifiedItemColumns("mi"), episodeCatalogBaseRelation, strings.Join(episodeConditions, " AND "))

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("hydrating mixed search items: %w", err)
	}
	defer rows.Close()
	return scanItems(rows)
}

const episodeSearchTitleExpr = `COALESCE(NULLIF(BTRIM(e.title), ''), 'Episode ' || e.episode_number::text)`

const episodeSearchTitleVector = `setweight(
	to_tsvector('simple', public.normalize_search_text(COALESCE(NULLIF(BTRIM(e.title), ''), 'Episode ' || e.episode_number::text))),
	'A'
)`

const episodeSearchOverviewVector = `to_tsvector('english', COALESCE(e.overview, ''))`

const mixedSearchOrder = `exact_title_match DESC, contiguous_title_match DESC, year_match DESC,
	phrase_rank DESC, title_rank DESC, title_prefix_rank DESC, overview_rank DESC,
	LOWER(title) ASC, content_id ASC`

// buildMixedSearchSQLFromParsed builds one ranked candidate set from the two
// physical catalog sources. The scored CTE deliberately carries only ranking
// fields; the wide MediaItem projection is hydrated after LIMIT/OFFSET so a
// broad match never sorts posters, arrays, or metadata blobs for every hit.
func (r *ItemRepository) buildMixedSearchSQLFromParsed(
	parsed parsedSearchQuery,
	itemTypes []string,
	limit, offset int,
	filter AccessFilter,
	includeTotal bool,
) (dataSQL, countSQL string, args []any) {
	searchText := searchTextFromParsed(parsed)
	if searchText == "" {
		return "", "", nil
	}

	mediaTypes, includeEpisodes := splitSearchItemTypes(itemTypes)
	includeMediaItems := len(itemTypes) == 0 || len(mediaTypes) > 0
	if !includeMediaItems && !includeEpisodes {
		return "", "", nil
	}

	args = []any{searchText, buildTitlePrefixTsQuery(searchText)}
	argIdx := 3
	var branches []string

	mediaConditions := []string{}
	if includeMediaItems {
		mediaTitleVector := `(
			setweight(to_tsvector('simple', public.normalize_search_text(COALESCE(mi.title, ''))), 'A') ||
			setweight(to_tsvector('simple', public.normalize_search_text(COALESCE(mi.original_title, ''))), 'A') ||
			setweight(to_tsvector('simple', public.normalize_search_text(COALESCE(mi.sort_title, ''))), 'B')
		)`
		mediaOverviewVector := `to_tsvector('english', COALESCE(mi.overview, ''))`
		mediaConditions = append(mediaConditions, searchMatchCondition(mediaTitleVector, mediaOverviewVector))
		if len(itemTypes) > 0 {
			mediaConditions = append(mediaConditions, fmt.Sprintf("mi.type = ANY($%d)", argIdx))
			args = append(args, mediaTypes)
			argIdx++
		}
		appendLibraryAccessConditions("mi.content_id", filter, &mediaConditions, &args, &argIdx)
		applyAccessFilter("mi", AccessFilter{
			MaxContentRating:   filter.MaxContentRating,
			ExcludedMediaTypes: filter.ExcludedMediaTypes,
		}, &mediaConditions, &args, &argIdx)
		mediaConditions = append(mediaConditions, MangaChapterExclusionWhere("mi"))
	}

	episodeConditions := []string{}
	if includeEpisodes {
		episodeConditions = append(episodeConditions,
			"si.type = 'series'",
			`EXISTS (SELECT 1 FROM episode_libraries available_el WHERE available_el.episode_id = e.content_id)`,
			searchMatchCondition(episodeSearchTitleVector, episodeSearchOverviewVector),
		)
		appendEpisodeLibrarySearchAccess("e.content_id", filter, &episodeConditions, &args, &argIdx)
		if filter.MaxContentRating != "" {
			allowedRatings := access.AllowedRatingsUpTo(filter.MaxContentRating)
			if len(allowedRatings) == 0 {
				episodeConditions = append(episodeConditions, "1 = 0")
			} else {
				episodeConditions = append(episodeConditions, fmt.Sprintf("si.content_rating = ANY($%d)", argIdx))
				args = append(args, allowedRatings)
				argIdx++
			}
		}
		if len(filter.ExcludedMediaTypes) > 0 {
			episodeConditions = append(episodeConditions, fmt.Sprintf("NOT ('episode' = ANY($%d))", argIdx))
			args = append(args, filter.ExcludedMediaTypes)
			argIdx++
		}
	}

	exactIdx := argIdx
	args = append(args, parsed.ExactTitleHint)
	argIdx++
	var yearArg any
	if parsed.Year != nil {
		yearArg = *parsed.Year
	}
	yearIdx := argIdx
	args = append(args, yearArg)
	argIdx++
	phraseIdx := argIdx
	args = append(args, parsed.Phrase)
	argIdx++

	if includeMediaItems {
		branches = append(branches, buildMixedSearchCandidateBranch(
			"mi.content_id", "mi.type", "mi.title", "mi.year",
			`(
				setweight(to_tsvector('simple', public.normalize_search_text(COALESCE(mi.title, ''))), 'A') ||
				setweight(to_tsvector('simple', public.normalize_search_text(COALESCE(mi.original_title, ''))), 'A') ||
				setweight(to_tsvector('simple', public.normalize_search_text(COALESCE(mi.sort_title, ''))), 'B')
			)`,
			`to_tsvector('english', COALESCE(mi.overview, ''))`,
			[]string{`mi.title_normalized`, `public.normalize_search_text(mi.original_title)`, `public.normalize_search_text(mi.sort_title)`},
			"media_items mi", mediaConditions, exactIdx, yearIdx, phraseIdx,
		))
	}
	if includeEpisodes {
		episodeNormalizedTitle := `public.normalize_search_text(` + episodeSearchTitleExpr + `)`
		branches = append(branches, buildMixedSearchCandidateBranch(
			"e.content_id", "'episode'::text", episodeSearchTitleExpr,
			"COALESCE(si.year, EXTRACT(YEAR FROM e.air_date)::integer, 0)",
			episodeSearchTitleVector, episodeSearchOverviewVector,
			[]string{episodeNormalizedTitle},
			"episodes e JOIN media_items si ON si.content_id = e.series_id",
			episodeConditions, exactIdx, yearIdx, phraseIdx,
		))
	}

	scoredCTE := "WITH scored AS (\n" + strings.Join(branches, "\nUNION ALL\n") + "\n)"
	statsCTE := `, stats AS (
		SELECT MAX(CASE WHEN title_rank > 0 OR title_prefix_rank > 0 THEN 1 ELSE 0 END) AS has_title_match
		FROM scored
	)`
	postFilter := fmt.Sprintf(`FROM scored
		CROSS JOIN stats
		WHERE scored.title_rank > 0
		   OR scored.title_prefix_rank > 0
		   OR (COALESCE(stats.has_title_match, 0) = 0 AND scored.overview_rank >= %g)`, overviewMatchFloor)

	pageTotalColumn := ""
	finalTotalColumn := ""
	if includeTotal {
		pageTotalColumn = ", COUNT(*) OVER () AS total_count"
		finalTotalColumn = ", page.total_count"
	}
	limitIdx, offsetIdx := argIdx, argIdx+1
	args = append(args, limit, offset)

	pageCTE := fmt.Sprintf(`, page AS (
		SELECT scored.*, ROW_NUMBER() OVER (ORDER BY %s) AS ordinal%s
		%s
		ORDER BY %s
		LIMIT $%d OFFSET $%d
	)`, mixedSearchOrder, pageTotalColumn, postFilter, mixedSearchOrder, limitIdx, offsetIdx)

	hydratedRelation := fmt.Sprintf(`LATERAL (
		SELECT %s
		FROM media_items hydrated_mi
		WHERE page.type <> 'episode'
		  AND hydrated_mi.content_id = page.content_id
		UNION ALL
		SELECT %s
		FROM %s
		WHERE page.type = 'episode'
		  AND mi.content_id = page.content_id
	) hydrated`, qualifiedItemColumns("hydrated_mi"), qualifiedItemColumns("mi"), episodeCatalogBaseRelation)

	dataSQL = scoredCTE + statsCTE + pageCTE + fmt.Sprintf(`
		SELECT %s%s
		FROM page
		JOIN %s ON true
		ORDER BY page.ordinal`, qualifiedItemColumns("hydrated"), finalTotalColumn, hydratedRelation)
	countSQL = scoredCTE + statsCTE + fmt.Sprintf("\nSELECT COUNT(*)\n%s", postFilter)
	return dataSQL, countSQL, args
}

func splitSearchItemTypes(itemTypes []string) (mediaTypes []string, includeEpisodes bool) {
	if len(itemTypes) == 0 {
		return nil, true
	}
	seen := make(map[string]struct{}, len(itemTypes))
	for _, itemType := range itemTypes {
		itemType = strings.ToLower(strings.TrimSpace(itemType))
		if itemType == "" {
			continue
		}
		if itemType == "episode" {
			includeEpisodes = true
			continue
		}
		if _, ok := seen[itemType]; ok {
			continue
		}
		seen[itemType] = struct{}{}
		mediaTypes = append(mediaTypes, itemType)
	}
	return mediaTypes, includeEpisodes
}

func searchMatchCondition(titleVector, overviewVector string) string {
	titleQuery := `websearch_to_tsquery('simple', public.normalize_search_text($1))`
	prefixQuery := `to_tsquery('simple', $2)`
	return fmt.Sprintf(`((%s) @@ %s OR ($2 <> '' AND (%s) @@ %s) OR (%s) @@ websearch_to_tsquery('english', $1))`,
		titleVector, titleQuery, titleVector, prefixQuery, overviewVector)
}

func buildMixedSearchCandidateBranch(
	contentIDExpr, typeExpr, titleExpr, yearExpr, titleVector, overviewVector string,
	exactTitleExprs []string,
	fromClause string,
	conditions []string,
	exactIdx, yearIdx, phraseIdx int,
) string {
	exactArms := make([]string, 0, len(exactTitleExprs))
	contiguousArms := make([]string, 0, len(exactTitleExprs))
	for _, expr := range exactTitleExprs {
		exactArms = append(exactArms, fmt.Sprintf("%s = $%d", expr, exactIdx))
		contiguousArms = append(contiguousArms, fmt.Sprintf("%s LIKE '%%' || $%d || '%%'", expr, exactIdx))
	}
	titleQuery := `websearch_to_tsquery('simple', public.normalize_search_text($1))`
	prefixQuery := `to_tsquery('simple', $2)`
	return fmt.Sprintf(`
		SELECT
			%s AS content_id,
			%s AS type,
			%s AS title,
			CASE WHEN $%d <> '' AND (%s) THEN 1 ELSE 0 END AS exact_title_match,
			CASE WHEN $%d <> '' AND (%s) THEN 1 ELSE 0 END AS contiguous_title_match,
			CASE WHEN $%d::int IS NOT NULL AND (%s) = $%d::int THEN 1 ELSE 0 END AS year_match,
			ts_rank_cd(%s, %s) AS title_rank,
			CASE WHEN $2 <> '' THEN ts_rank_cd(%s, %s) ELSE 0 END AS title_prefix_rank,
			ts_rank_cd(%s, websearch_to_tsquery('english', $1)) AS overview_rank,
			CASE WHEN $%d <> '' THEN ts_rank_cd(%s, phraseto_tsquery('simple', public.normalize_search_text($%d))) ELSE 0 END AS phrase_rank
		FROM %s
		WHERE %s`,
		contentIDExpr, typeExpr, titleExpr,
		exactIdx, strings.Join(exactArms, " OR "),
		exactIdx, strings.Join(contiguousArms, " OR "),
		yearIdx, yearExpr, yearIdx,
		titleVector, titleQuery,
		titleVector, prefixQuery,
		overviewVector,
		phraseIdx, titleVector, phraseIdx,
		fromClause, strings.Join(conditions, " AND "))
}

func appendEpisodeLibrarySearchAccess(
	episodeIDExpr string,
	filter AccessFilter,
	conditions *[]string,
	args *[]any,
	argIdx *int,
) {
	if filter.AllowedLibraryIDs != nil {
		*conditions = append(*conditions, fmt.Sprintf(
			"EXISTS (SELECT 1 FROM episode_libraries allowed_el WHERE allowed_el.episode_id = %s AND allowed_el.media_folder_id = ANY($%d))",
			episodeIDExpr, *argIdx))
		*args = append(*args, filter.AllowedLibraryIDs)
		*argIdx++
	}
	if len(filter.DisabledLibraryIDs) > 0 {
		*conditions = append(*conditions, fmt.Sprintf(
			"NOT EXISTS (SELECT 1 FROM episode_libraries disabled_el WHERE disabled_el.episode_id = %s AND disabled_el.media_folder_id = ANY($%d))",
			episodeIDExpr, *argIdx))
		*args = append(*args, filter.DisabledLibraryIDs)
		*argIdx++
	}
}
