# Catalog API

## Version liveness

`FileVersion.available` reports the durable per-version health signal on the
item detail and item-versions responses: a virtual candidate is available when
it has not been stamped failed (`failed_at` is NULL), a local file when it is
not marked missing (`missing_since` is NULL). The field is omitted when the
version is available, so an absent `available` means available/unknown; `false`
means the version is currently unavailable. `FileVersion.failed` remains the
virtual-only "produced no bytes at stream-open" flag.

`POST /api/v1/catalog/versions/check` batch-tests a set of media file IDs and
stamps the durable signal. The request is:

```json
{
  "file_ids": [123, 456]
}
```

At most 40 IDs are accepted per request (413 `too_large` beyond that; 400
`bad_request` when the body is missing or the list is empty). Each file is
tested cheaply — virtual rows resolve their pinned `?result=` candidate through
the provider (no media transfer), local rows are read from `missing_since` with
no probe — with bounded concurrency and a per-file timeout. A confirmed dead
pin stamps `failed_at`; a successful resolution clears it. Ambiguous provider
errors (provider down, timeout) leave the stamp unchanged and report the row's
current computed availability, so a provider outage cannot mass-tag versions.

**Response** (200 OK):

```json
{
  "results": [
    { "file_id": 123, "available": true },
    { "file_id": 456, "available": false }
  ]
}
```

Unknown or deleted file IDs are reported as `available: false`.

## Multi-audio language support and release metadata

MULTi/DUAL releases — a single audio stream tagged `und`/`mul`/empty whose
languages live in the track title (e.g. "English / French / Spanish") — now
carry real language identity instead of being collapsed to a single code.

- **`FileVersion.audio_tracks[].languages`** — the full advertised language
  list for a MULTi/DUAL track, parsed from the track title at probe time when
  the container language tag is absent, undetermined, or multiple. `language`
  keeps the primary code (the first concrete one when the tag was
  undetermined). Rows probed before this support are backfilled at read time
  from the embedded title until the probe-version re-probe catches up.
- **`FileVersion.audio_tracks[].index`** — the container stream ordinal
  (ffmpeg's `0:a:N`). Track list order can differ from container order on
  MULTi releases, so this is the identity a client should use when it needs
  the real stream position.
- **`FileVersion.release_name`** / **`FileVersion.release_group`** — the file
  stem (basename without extension) and the trailing group tag on
  release-style names (`Movie.2023.2160p.AltMount` → group `AltMount`), so
  clients can show which release a version is. `release_group` is empty when no
  group tag is present.

`GET /api/v1/catalog/filters` reports the language facets on
`audio_languages` and `subtitle_languages` (alongside `resolutions`) when
`include_technical` is true (the default). A MULTi track satisfies the
audio-language browse filter for any of its `languages[]` codes, not just its
primary `language`.

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
