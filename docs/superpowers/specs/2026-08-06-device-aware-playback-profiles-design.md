# Device-Aware Playback Profiles for Virtual Stream Ranking

## Problem

Virtual playback candidates (debrid/indexer results surfaced through the
`virtual://` resolver) are ranked by resolution label only. A device that
cannot direct-play the winning candidate — an Apple TV that cannot play the
picked H.265/DV container, or a phone whose audio sink rejects TrueHD — gets
a stream that forces a transcode or fails, even when a direct-playable
candidate existed in the same provider response.

Three gaps cause this today:

1. **No persistent per-device capability profile.** The Jellyfin compat layer
   keeps the last reported `DeviceProfile` in memory keyed by compat session
   token (`internal/jellycompat/deviceprofile.go`), which expires after 6h and
   is invisible to native `/api/v1` playback. Native clients never report codec
   capability at all.
2. **Candidate selection ignores device capability.** `selectVirtualCandidate`
   (`internal/plugins/virtual_playback.go:852`) matches only `profile` /
   resolution labels; `filterVirtualPlaybackStreams`
   (`internal/api/handlers/playback_virtual.go:551`) filters identity, not
   capability. Neither consults a device profile.
3. **The best-result cache is device-blind.** `bestResultCacheKey`
   (`internal/api/handlers/playback_virtual.go:120`) hashes
   `(contentID, neutralURI, ownerInstallationID)` only, so the first device to
   play an item pins the cached `result=` for every other device.

## Decision

The server will auto-create a **per-device playback profile** per
`(profileID, deviceID)` and use it to rank virtual candidates so the
best-quality **direct-playable** candidate wins, with the current
resolution-label selection as an exact fallback when no profile is known.

The capability profile is seeded automatically from every device that reports
one:

- **Jellyfin clients** already send `DeviceProfile` in the capabilities and
  playback-info handshakes. Those go into `DeviceProfileStore` today keyed by
  compat token; we additionally persist them into the new store keyed by
  `(profileID, clientDeviceID)` (both are already on the compat
  `PlaybackSession`, and `PseudoUserID` already encodes `(userID, profileID)`).
- **Native `/api/v1` clients** (web, Apple, Android) report codec capability
  once through a new additive endpoint; until they do, the registry keeps a
  conservative `DefaultDeviceProfile()`-style profile that never removes
  candidates that were selectable before.

Ranking happens in one place — a pure scorer next to the existing
`DeviceProfile` code — and is applied at the two decision points that already
exist:

1. `selectVirtualCandidate` (plugin/`virtual://` path) re-ranks the provider
   candidates by score before the existing resolution-label fallback.
2. `resolveVirtualPlaybackSource` (handler path) sorts
   `filterVirtualPlaybackStreams` output by score before resolve+probe, so
   failover order is also device-aware.

The best-result cache is **not** split per device. Per Google AI Pro's
review, provider candidate lists for one `contentID` are identical for every
device, so a fingerprint in the key would fragment hits and defeat detail-page
pre-warming. Instead the cache stores the filtered, device-neutral candidate
list under the existing `(contentID, neutralURI, ownerInstallationID)` key, and
`RankForDevice` runs in memory on every hit. A TV and a phone therefore share
one cache entry but each ranks it for itself.

## Architecture and Data Flow

### New persisted store

A Goose migration (created with `make migrate-create NAME=add_device_profiles`)
adds `user_device_profiles`:

- `user_id`, `profile_id`, `device_id`, `direct_play_profiles jsonb NOT NULL
  DEFAULT '[]'`, `codec_profiles jsonb NOT NULL DEFAULT '[]'`,
  `transcoding_profiles jsonb NOT NULL DEFAULT '[]'`,
  `max_streaming_bitrate bigint NOT NULL DEFAULT 0`, `source text NOT NULL
  DEFAULT 'client'` (`client` | `admin` | `seed`), `updated_at timestamptz`,
  `last_reported_at timestamptz`.
- Primary key `(user_id, profile_id, device_id)`, same FK shape as
  `user_devices` (`180_user_devices.sql` is the reference migration).
- Rows are created lazily on first capability report; the existing
  `user_devices` registry row is updated in the same transaction so the device
  appears in the Devices admin page without a separate write.

The store is a new `DeviceProfileRegistry` on `userstore.UserStore`
(interface in `internal/userstore/store.go`, Postgres implementation in
`internal/userstore/pgstore/`), with `GetProfile`, `PutProfile`, and
`ListProfiles` mirroring the existing `DeviceRegistry` methods and the
`storetest` suite pattern. Only the persisted Jellyfin `DeviceProfile` fields
are stored — no transient tokens, no URLs.

### Capability fingerprint

`Fingerprint(profile) -> string` is a canonical, sorted serialization of the
normalized capability fields (stable JSON, sorted arrays). It is not part of
the best-result cache key (cache stays device-blind per Google AI Pro's
review); it is:

- the equality signal that suppresses redundant DB writes on re-report;
- exposed as `capability_fingerprint` on the devices API so clients and admin
  UI can see when a stored profile changed, and clients can detect when a
  re-report is needed.

### Scoring model

A pure function `RankForDevice(profile, candidates) (ranked, directPlayOK)`
lives in `internal/jellycompat` (new file `candidate_rank.go`) next to the
existing `DeviceProfile.SupportsDirectPlay` logic, operating on a minimal
`CandidateCodecs` struct (video codec, audio codec, HDR, container, resolution
label, bitrate) so neither `internal/plugins` nor `internal/api/handlers`
needs to import the other's candidate type. Each candidate converts to
`CandidateCodecs` at the call site.

Scoring order (highest wins):

1. **Direct-playability** (binary): reuse the existing
   `SupportsDirectPlay`/`codecProfileCompatibility` semantics — the candidate's
   container, video codec, audio codec, and HDR must all be accepted by the
   profile. No profile known, or profile empty: treat as direct-play OK so the
   pre-feature behavior is unchanged.
2. **Quality tier** among direct-playable candidates: resolution label rank
   (2160p > 1080p > 720p > 480p), then HDR (HDR10/DV preferred when the
   profile accepts it, otherwise SDR preferred), then bitrate, then file size.
   The existing `sortByResolution` web helper is the frontend mirror of the
   same order.
3. **Stability tie-break**: candidates with reliable declared codecs
   (`hasReliableCodecs`) sort above metadata-free ones; provider order is the
   final tie-break so behavior stays deterministic.

When no capability profile exists for the device, the scorer returns the
candidates in the current resolution-label order, which is exactly what
`selectVirtualCandidate` does today — the feature degrades to today's behavior
with zero behavior change.

### Cache change (per Google AI Pro review)

`bestResultCacheKey(contentID, neutralURI, ownerInstallationID)` stays
unchanged. `VirtualBestResultCache` entries change from a single `resultURI`
to the filtered, device-neutral `[]VirtualPlaybackStream` list (same TTL/LRU
bounds). The device-blind URI fast-path is removed: every replay ranks the
cached list for the requesting device before resolve+probe. Pre-warm reuse
(`resolveVirtualPlaybackSource:204`) stores its single chosen stream as a
one-element list; it was already ranked for the device that viewed the detail
page, so replay for the same profile re-ranks it identically.

### v1 additive API

Per the v1 API rules (additive-only, feature detection):

- `PUT /api/v1/devices/{device_id}/capabilities` — body is the normalized
  capability object: `codecs_video`, `codecs_audio`, `containers`,
  `max_resolution`, `hdr`. It persists to the registry keyed by the
  authenticated `(profileID, deviceID)` and returns
  `{ "capability_fingerprint": "..." }` so clients can detect when a
  re-report is needed. The web client computes the payload with
  `canPlayType`/`isTypeSupported` and self-dedupes with a stored fingerprint.
- `GET /api/v1/devices` — each device gains `capability_fingerprint` and
  `capability_source` (omitted when absent) so clients can show "using
  auto-detected profile".
- `GET /api/v1/capabilities` — new feature-detection endpoint advertising
  `device_capabilities_reporting: true` (the repo requires capability
  endpoints for feature detection rather than version sniffing).
- `DELETE /api/v1/devices/{device_id}` (forget) also deletes the profile row,
  and the existing `DELETE .../settings` keeps the profile so forgetting and
  clearing stay distinct.

### Web changes

- `web/src/api/client.ts` already sends `X-Silo-Device-Id/-Name/-Platform` on
  every request; add a capability report after login/profile load (and when
  the fingerprint changes) so every browser device gets a profile
  automatically. Per Google AI Pro's review, capabilities are detected with
  `HTMLMediaElement.canPlayType()` and `MediaSource.isTypeSupported()`
  (Safari reports HEVC/DV, Chrome reports AV1/VP9) — never a hardcoded
  H.264/AAC table, which would penalize capable browsers.
- Devices page (`web/src/hooks/queries/devices.ts`,
  `web/src/pages/AdminDevices.tsx`): show `capability_fingerprint`/
  `capability_source` per device; household parent can clear a device's
  profile (reuse the existing clear/forget controls).
- `VersionFlyout`/`VersionDropdown`: when the device has a profile, the picker
  sorts with "Best for this device" first using the same quality order, and
  marks the auto-selected direct-play winner.

### Wiring

- `cmd/silo/main.go` constructs the new registry store and passes it to both
  the `PlaybackHandler` (native path) and the jellycompat server (via a small
  `DeviceProfilePersister` interface so `internal/jellycompat` never depends
  on `internal/api`).
- jellycompat `HandleCapabilitiesFull` and the playback-info handshake
  (`handlers_playback.go:625,797`) call the persister after the existing
  `DeviceProfileStore.Put`, bridging `(PseudoUserID → profileID,
  ClientDeviceID → deviceID)`.
- Native playback (`resolveVirtualPlaybackSource`) reads the profile through a
  small in-memory TTL cache (5 min) in front of the registry, so Postgres is
  never on the playback critical path (per Google AI Pro's review); DB-less/
  test mode falls back to nil registry = permissive behavior.
- The ranking reuses the existing device-capability scoring already shipped in
  `internal/plugins/virtual_prewarm.go` (`DeviceCapabilities`,
  `scoreCandidate`, `selectBestCandidateForDevice`), fed today by the ad-hoc
  `X-Device-Codecs-Video/-Audio/-Containers/-Max-Resolution/-HDR` headers
  (`triggerPrewarm`). The persisted profile uses that same normalized
  capability shape, so prewarm, the detail-page picker, and the playback path
  all rank with one scorer. The Jellyfin `DeviceProfile` handshake is
  converted to this normalized shape when persisted; the compat layer keeps
  its precise `SupportsDirectPlay`/`codecProfileCompatibility` negotiation for
  session-time decisions.

## Compatibility and Error Handling

- **Additive only.** No existing endpoint changes shape, status, or meaning;
  new fields are optional and omitted when absent. Devices without a profile
  get today's exact selection.
- **Jellyfin path is unchanged** for sessions that never persist a profile;
  the in-memory `DeviceProfileStore` remains the source of truth for a
  session's immediate negotiation, and the new registry is an additional,
  longer-lived store for candidate ranking.
- **Stale profiles.** A device that changes (new OS, new codec support) can
  re-report; fingerprint comparison makes the update a single upsert. A
  profile that is older than 90 days is treated as absent for ranking (and a
  lazy re-report refreshes it), so a device's aging capabilities never lock it
  out of new codecs.
- **Per-device privacy.** Profiles are per-`(user, profile, device)`, listed
  only through the existing Devices scoping rules (`DeviceHandler` filters to
  the caller's profile; household scope requires the parent guard).
- **DB errors** never fail playback: a registry read error falls back to the
  permissive default and is logged, exactly like the existing best-result
  cache behavior.

## Verification

Unit tests (Go):

1. `RankForDevice` orders direct-playable above non-direct-playable, quality
   within direct-play, and preserves current resolution-label order when the
   profile is empty/default.
2. `SupportsDirectPlay` reuse: a profile that rejects H.265 picks an H.264
   candidate; a profile that rejects TrueHD picks an AAC candidate; DV handling
   matches the existing DV/4K gate.
3. `bestResultCacheKey` differs per capability fingerprint; identical
   fingerprints collide.
4. Registry store tests in `storetest` (upsert, fingerprint-dedup, delete on
   forget, scope filtering).

Handler tests:

5. `resolveVirtualPlaybackSource` with a profile selects the direct-playable
   candidate and caches under the device-aware key.
6. Jellyfin handshake persists a profile to the registry once per
   `(profileID, deviceID)`.

Web tests:

7. `VersionFlyout` sorts "Best for this device" first only when a profile
   exists; devices API types gain the new optional fields.

End-to-end sanity:

8. A TV-profile device and a phone-profile device playing the same item pick
   different candidates when the provider offers both, and replay uses each
   device's own cache entry.

## Security and Operational Impact

- The new endpoint is authenticated and profile-scoped like every other
  `/api/v1/devices` route; a profile row can only be written for a device the
  caller can manage (same guard as device settings).
- Payloads are validated with the same codec/container constraints the compat
  layer already enforces; values are stored as JSONB with size caps.
- Cache isolation slightly increases the best-result cache working set in the
  worst case (one entry per device fingerprint); the existing TTL and LRU cap
  bound it, and the fingerprint collapse keeps identical devices sharing
  entries.
- Rollback is a code revert plus dropping the new migration; the feature is
  inert (permissive default) whenever the registry is unavailable, so an
  operator can disable it without a schema change by leaving the registry
  unwired.

## Non-Goals

- No live TV, OTA, IPTV, DVR, EPG, or `.strm` support — per
  `docs/non-goals.md`, these remain out of scope.
- No automatic transcoding decisions; this design only ranks candidates.
  Transcode negotiation continues to be owned by the existing playback/recipe
  paths.
- No client SDK changes: the Apple/Android clients can adopt the new endpoint
  incrementally, and until they do they are treated as permissive devices
  exactly as today.
