# Catalog API

## Saved browse sort

`PUT /api/v1/collections/sort-preference` saves the active profile's sort for a
library collection, user collection, Watchlist, or Favorites. The request is:

```json
{
  "collection_kind": "watchlist",
  "field": "added_at",
  "order": "desc"
}
```

`collection_kind` accepts `library`, `user`, `watchlist`, or `favorites`.
`collection_id` is required for collection kinds and is omitted or ignored for
Watchlist and Favorites. Saved personal-list preferences accept non-personalized sort
fields; `added_at` means the date the item was
added to the list. Personalized sorts (`progress`, `date_viewed`, and `plays`)
are rejected for both saved preferences and Favorites/Watchlist browse. History
accepts `date_viewed` with an active profile, but rejects mutable `progress` and
`plays` sorts. An empty `field` pins the profile to list
source order. `DELETE /api/v1/collections/sort-preference?collection_kind=watchlist`
removes the saved preference. Collection kinds also require `collection_id` on
DELETE.

When a catalog request has no explicit sort, its saved preference is applied
before the source default. `/api/v1/catalog` reports an applied saved/default
sort as `effective_sort`; source order omits that field. `effective_sort` is
reported the same way for `group=work` requests, and `sort_metrics` on each item
describes the effective sort rather than the (possibly empty) requested one.

## Feature detection

`GET /api/v1/collections/capabilities` returns `sort_preference_kinds`, the
`collection_kind` values this server accepts, and `admin_item_materialize`:

```json
{
  "sort_preference_kinds": ["library", "user", "watchlist", "favorites"],
  "admin_item_materialize": true
}
```

Check it before saving a Watchlist or Favorites preference. The older
`collection_sort_preferences` boolean is also true on servers that predate the
personal-list kinds and reject them with a 400, so it cannot be used to detect
them. When `sort_preference_kinds` is absent, assume `library` and `user` only.
`admin_item_materialize` indicates support for
`POST /api/v1/admin/collections/{id}/materialize/{item_id}` to repair or
materialize virtual placeholder files for a collection item.

## Admin item materialization

`POST /api/v1/admin/collections/{id}/materialize/{item_id}`

Requires administrator authentication. Idempotently establishes or repairs
virtual playback files, profile variants, and released episodes for a
collection item.

`files_created` and `files_existing` count distinct virtual file identities
across the base item and released episode variants for this operation. A file
identity is the `(owner installation, target library, virtual URI)` tuple.
`episodes_materialized` counts distinct released episodes represented by the
result, not the number of provider/profile files. Repeating the request reports
the same episode count and moves files from `files_created` to
`files_existing`.

**Response** (200 OK):

```json
{
  "success": true,
  "content_id": "movie-tmdb-12345",
  "media_type": "movie",
  "files_created": 1,
  "files_existing": 0,
  "episodes_materialized": 0,
  "message": "Materialized 1 virtual files (0 existing) for movie-tmdb-12345 (movie)"
}
```

**Status Codes**:
- `200 OK`: Item was successfully materialized or verified (idempotent).
- `400 Bad Request`: Ineligible media type, incompatible target library, or virtual playback is disabled on the collection.
- `401 Unauthorized`: Missing or invalid authentication token.
- `403 Forbidden`: Authenticated user is not an administrator.
- `404 Not Found`: Collection or item not found, or item is not a member of the collection.
- `503 Service Unavailable`: Upstream virtual provider plugin is unavailable or misconfigured.

**Sync staging**: during collection sync, newly prepared files stay hidden
until membership acceptance commits. Removing a collection member only deletes
catalog rows proven to be collection-created virtual state; ordinary
metadata-only entries survive as catalog rows.

## History ordering

History defaults to chronological watch-event order. Explicit `date_viewed`
sorting uses each displayed item's latest visible history event, including
episode events collapsed into their parent series; it does not require a
completed watch. `order=asc` puts the oldest latest watch first, and `desc`
puts the newest first. Library/media-scope/search overlays retain this order
before pagination. History does not currently support saved sort preferences.
