# NimoOS-Photos

NimoOS's photo album service, providing **photo/video indexing, EXIF parsing, thumbnail generation, face recognition, reverse geocoding, and CLIP semantic search**. Current version `v1.9.0-alpha1`.

Binds to a random localhost port, forwarded by Gateway, API prefix `/v1/photos`; TUS resumable upload prefix `/v1/upload-tus`.

---

## Overall architecture

```
External request (forwarded by Gateway, /v1/photos/* and /v1/upload-tus/*)
                        │
                        ▼
        ┌───────────────────────────────────────┐
        │   nimoos-photos.service (Go, Echo)    │
        │   - JWT auth (with exemption rules)   │
        │   - Asset CRUD / timeline / albums     │
        │   - Favorites / trash / persons / places│
        │   - Smart View semantic auto-albums    │
        │   - TUS resumable upload (/DATA/Gallery)│
        └───────────────────────────────────────┘
               │           │            │
    ┌──────────▼──┐  ┌──────▼────────┐  │
    │  Watcher    │  │   Indexer     │  │
    │ (fsnotify)  │→─│ (3 workers)   │  │
    │ /DATA/Gallery│  │ SHA-256 dedup │  │
    │ /DATA/...   │  └──────┬────────┘  │
    └─────────────┘         │           │
                     ┌──────▼───────────▼──────────────┐
                     │   SQLite (WAL) + sqlite-vec       │
                     │  assets / asset_exif             │
                     │  face_detections / persons        │
                     │  clip_embeddings (vec0, 1152-dim) │
                     │  asset_ocr / asset_geo            │
                     │  albums / smart_views / ...       │
                     └───────────────────────────────────┘
                                    │
                     ┌──────────────▼──────────────────────────┐
                     │  nimoos-photos-ml-server (Docker)       │
                     │  127.0.0.1:3003  /predict               │
                     │  - CLIP ViT-SO400M-16-SigLIP2-384 (1152-dim)│
                     │  - Face detection+recognition antelopev2 (512-dim)│
                     │  - OCR PP-OCRv5_server                  │
                     └───────────────────────────────────────┘
```

The ML backend is an independent Docker Compose stack (`deploy/ml/docker-compose.yml`), bound to `127.0.0.1:3003`, with the image bundled offline (not pulled over the network).

---

## API routes (`/v1/photos`)

| Method | Path | Purpose |
|---|---|---|
| GET | `/assets` | List assets (supports pagination, place/location filtering) |
| POST | `/assets/upload` | Regular upload (small files) |
| GET | `/assets/:id` | Get a single asset |
| DELETE | `/assets/:id` | Delete an asset |
| GET | `/assets/:id/thumbnail` | Get thumbnail (JWT-exempt) |
| GET | `/assets/:id/original` | Get the original file (JWT-exempt) |
| GET | `/assets/:id/live` | Get Live Photo video (JWT-exempt) |
| GET | `/assets/:id/sprite` | Video hover-preview sprite (JWT-exempt) |
| GET | `/assets/:id/ocr` | OCR line text + normalized coordinates (`?q=` returns only matching lines, using the same matching rules as /search/smart's OCR; used for front-end search-hit highlighting; JWT-protected) |
| GET | `/timeline` | Timeline grouped by year/month. **Deprecated for scale**: kept for backward compatibility, but it scans the whole table and groups in Go, so cost grows linearly with library size (benchmarked at ~571.7ms for 100k assets). New consumers should use `/timeline/buckets` + `/timeline/bucket` instead |
| GET | `/timeline/buckets` | Month-bucket directory: one row per `YYYY-MM` (grouped by `strftime('%Y-%m', COALESCE(taken_at, indexed_at))`) with its asset count, ordered newest-first; the unknown-date bucket (`taken_at`/`indexed_at` both null) sorts last. Backed by `idx_assets_monthkey`, so it's an index scan rather than a table scan |
| GET | `/timeline/bucket` | Paginated assets within a single month bucket (`?year=&month=&limit=&offset=`; `year=0&month=0` selects the unknown-date bucket). `limit` defaults/caps at 500, `offset` floors at 0, `year<0 \|\| month<0 \|\| month>12` returns 400. Same column set/enrichment as `/timeline` (EXIF, favorited_at, OCR-hit flag, named faces, place names) but scoped to one month via an equality match on `idx_assets_monthkey`, so it stays flat-cost regardless of library size (benchmarked at ~1.04ms/page for 100k assets, a page selected via `year=2020,month=6` — vs. ~571.7ms for the legacy full `/timeline` scan, roughly a 549× speedup) |
| POST | `/search/smart` | CLIP semantic search + OCR exact match (localhost MCP calls are JWT-exempt, see Authentication) |
| GET | `/search/faces/:person_id` | Find assets by person |
| GET/POST/DELETE | `/albums[/:id]` | Album CRUD (`GET /albums` is JWT-exempt for localhost MCP calls, see Authentication) |
| GET | `/albums/:id/summary` | Album summary |
| POST/DELETE | `/albums/:id/assets[/batch]` | Add/remove album assets |
| PATCH | `/albums/:id` | Update album metadata |
| PATCH | `/albums/:id/assets/order` | Manual reordering |
| GET | `/places` | List of places (aggregated by city) |
| GET | `/places/:key` | Details for a given place |
| GET | `/places/:key/cover-candidates` | Cover candidates |
| PUT/DELETE | `/places/:key/cover` | Set/reset place cover |
| PUT/DELETE | `/places/:key/spot-name` | Set/reset spot name |
| POST | `/places/:key/album` | Create an album from a place |
| POST/DELETE | `/favorites/:asset_id` | Favorite/unfavorite |
| GET | `/favorites[/ids/export/top]` | Favorites list/IDs/export/top. `GET /favorites` list defaults/caps `limit` at 500 (an unspecified or out-of-range `limit` is normalized to 500, not "unlimited"); `/export` is unaffected and still returns the full set |
| GET | `/trash` | Trash. `limit`/`offset` are handler-parsed and normalized the same way: `limit<=0 \|\| limit>500` → 500, `offset<0` → 0 |
| POST | `/trash/restore` | Batch restore |
| POST | `/trash/empty` | Empty trash |
| POST/DELETE | `/trash/:id/restore` | Restore/permanently delete a single item |
| POST | `/views/:asset_id` | Record a view |
| GET | `/persons` | List of persons (face clusters). Ordering: named/favorited persons first, then by photo count descending — see Core flows §3 "Listing order contract" |
| GET | `/persons/hidden` | List of plainly-hidden persons (management view for the hidden-people surface); excludes persons currently in the purge grace period (those are "being deleted", not "hidden" — see Core flows §3). Registered **ahead of** `GET /persons/:id` in `route/router.go`, or `hidden` would be swallowed as an `:id` lookup |
| GET | `/persons/merge-suggestions` | Merge suggestions (legacy, on-the-fly, named-centroid pairs only) |
| POST | `/persons/merge-suggestions/reject` | Reject a merge suggestion (legacy) |
| GET | `/persons/merge-suggestions/v2` | Cluster-merge questions: every open, durable gray-band candidate (see Core flows §3 "Cluster-merge questions"), dist ascending, hidden persons excluded on either side. `{"pairs":[{"id","dist","from":<Person>,"into":<Person>,"fromFaceIds":[≤4],"intoFaceIds":[≤4],"fromFaces":[≤4 {"faceId","assetId"}],"intoFaces":[≤4 {"faceId","assetId"}]}]}` — face id arrays and their `fromFaces`/`intoFaces` counterparts are the same quality-ordered preview faces, same order (exemplar-first, falling back to score order for auto persons, which never carry exemplar flags); `fromFaces`/`intoFaces` additionally carry each face's asset id so the merge-card UI can open the full photo behind a preview face; always `[]`, never `null`, when a side has no active member faces. Registered **ahead of** `GET /persons/:id`, same route-order trap as `/persons/hidden` |
| POST | `/persons/merge-suggestions/v2/:id/accept` | Accept a cluster-merge question: merges `from` into `into` (same tx-scoped machinery as `POST /persons/merge`) and marks the row `accepted`. Idempotent |
| POST | `/persons/merge-suggestions/v2/:id/reject` | Reject a cluster-merge question: marks the row `rejected` and pins a durable `face_negative_pairs` cannot-link between the two clusters' top-quality representative faces, so the pairing is suppressed on future passes even after the (likely auto, id-unstable) persons get rebuilt. Idempotent |
| POST | `/persons/merge` | Merge persons (400 if `from_id==into_id`; 404 if either person doesn't exist) |
| POST | `/persons/recluster` | Re-cluster |
| GET | `/persons/suggestions` | List every open join/review suggestion (the exemplar-assignment gray-zone queue — see Core flows §3 "Exemplar templates + KNN assignment"), grouped by visible person. Each group also carries `exemplarFaceIds`: up to 5 of that person's quality-gated exemplar faces (`face_person.exemplar=1`), ordered by `face_detections.score DESC` (NULLs last) then face id for determinism, with the person's `cover_face_id` excluded (the review wizard's header shows the cover separately) — always `[]`, never `null`, when the person has no exemplars. Registered **ahead of** `GET /persons/:id`, same route-order trap as `/persons/hidden` |
| POST | `/persons/suggestions/:id/accept` | Accept one open suggestion: `join` inserts (or `review` upgrades) the `face_person` row with `confirmed=1`, always winning even if the face auto-joined a *different* person meanwhile (both persons' stats get recomputed). Idempotent: a repeat call on an already-decided suggestion 200s with its current state rather than erroring |
| POST | `/persons/suggestions/:id/reject` | Reject one open suggestion: `join` just records a `person_negatives` row (the face was never a member); `review` also detaches the existing membership first. Idempotent like accept |
| POST | `/persons/suggestions/batch` | `{ "accept": [ids], "reject": [ids] }` → `{ "results": { "<id>": {"status","decidedAt"?,"error"?} } }`. Every id is its own transaction (one failure never rolls back another), always 200 with the per-id outcome map; an id in both arrays resolves accept-then-reject |
| GET | `/persons/calibration` | Threshold self-calibration status: each of the five calibratable keys' effective value + resolution source (`conf`/`calibrated`/`profile`/`code`), the stored profile version, and each tier's (knn/merge/twopass) evidence-bar progress + last-run outcome — see Core flows §3 "Threshold self-calibration". Registered ahead of `GET /persons/:id`, same route-order trap as `/persons/hidden` |
| GET | `/persons/calibration/history` | Up to `?limit=` (default 50, capped 500) calibration history rows, newest first (tier/outcome/truth counts/old+new values) |
| PUT | `/persons/calibration/profile` | Hot-update the calibration factory profile (bands + step/cooldown rules); validated, strictly-increasing version; takes effect immediately, no restart |
| GET/PUT/DELETE | `/persons/:id` | Person detail/update/delete (`DELETE` default = undoable hide + grace-period purge, `?purge=true` = immediate hard delete — see Core flows §3 "Person delete semantics") |
| POST | `/persons/:id/hide` | Explicit plain hide: sets `hidden=1` with no `purge_at`, so the grace-period purge sweep never picks this row up. Distinct from `DELETE`'s default path (which schedules a hard purge) — this is a durable hide, not a pending delete |
| POST | `/persons/:id/restore` | Restore a deleted **or** plainly-hidden person, cancelling any pending grace-period purge |
| GET | `/persons/:id/assets` | Person's photos (404 if the person is hidden or missing, via the `PersonVisible` guard) |
| GET | `/persons/:id/relations` | Related persons (404 if the person is hidden or missing, via `PersonVisible`) |
| GET | `/persons/:id/places` | Places this person appears in (404 if the person is hidden or missing, via `PersonVisible`) |
| GET | `/persons/:id/face-thumbnail` | Person face thumbnail (JWT-exempt; crops in raw pixel coordinates and rotates per `asset_exif.orientation` at serve time; falls back to a live face if the cover asset is trashed/offline) |
| GET | `/faces/:id/thumbnail` | Cropped square thumbnail for an **arbitrary** `face_detections` id, independent of person membership (JWT-exempt) — needed by the suggestions inbox, whose candidate faces may be free-floating (not, or no longer, attached to any person). 404s for an unknown face id or an offline/deleted owning asset |
| POST | `/persons/:id/detach` | Detach a specific face from this person |
| GET | `/status` | Indexing status (pending/indexed/queue counts) |
| POST | `/scan` | Manually trigger a directory scan |
| GET | `/tasks` | Current task list (index/embedding/ocr) |
| GET/PUT | `/config` | Album configuration (WatchDirs, feature toggles, etc.) |
| GET | `/storage` | Storage stats. DB-derived buckets (Photos/Videos/Raw/AI/Disk) come from a single SQL aggregate query and are always computed fresh on every call (no cache in front of them anymore). FS-derived buckets (CacheBytes/PrunableBytes, from walking `thumbs/`/`face-thumbs/`) are stale-while-revalidate: the request returns immediately with the last cached value (or zero on first call) while a background walk (`refreshFS`, single-flighted via an in-flight flag) refreshes it for the next call; `main.go` also calls `WarmFS()` once at startup so the settings page isn't stuck on zero right after boot. `Prune()` still invalidates the FS cache, triggering a fresh walk on the next `Stats()`/`WarmFS()` call |
| POST | `/cache/prune` | Clean up orphaned thumbnails + orphaned face-thumbs + expired upload staging (same implementation as the daily scheduled cleanup) |
| POST | `/index/rebuild` | Rebuild the vector index |
| GET | `/about` | Version/ML status information |
| GET/POST/PUT/DELETE | `/smart-views[/:id]` | Semantic auto-album CRUD |
| POST | `/smart-views/preview` | Preview Smart View results |
| GET | `/smart-views/:id/assets` | Smart View asset list |
| GET | `/smart-views/:id/activity` | Smart View activity |
| POST | `/smart-views/:id/export` | Export a Smart View |
| POST | `/smart-views/:id/duplicate` | Duplicate a Smart View |

**TUS routes** (registered at `/v1/upload-tus`, independent of the group above):
- `ANY /v1/upload-tus` and `ANY /v1/upload-tus/*`: TUS v2 resumable upload (POST/PATCH/HEAD/OPTIONS), 20 GB max per file, staging directory `<DataPath>/tus-staging` (follows `DataPath` in `photos.conf`, migrates with derived data onto the same disk; created with `0700` permissions, reset by `main.go` after `config.Init` and before any consumer), auto-cleaned after 7 days. The old fixed path `/DATA/.system_data/photos-tus-staging` (`common.LegacyStagingDir`) is deprecated — it's only swept once, at service **startup**, as a fallback for historical leftovers, and never used after that; subsequent periodic cleanup only scans the new directory (see "Daily cache cleanup" below).

---

## Core flows

### 1. Photo ingestion (scan / watch)

```
fsnotify (Watcher)
  Create/Write → isSupportedMedia → Indexer.Enqueue(path)

MessageBus subscription to nimoos:media:created (service/buscreated.go)
  Index as soon as data lands: files are enqueued directly (seen dedup is idempotent), directories are recursively expanded via walkSupported

ScanDirectory (manual/startup) → walkSupported → processFile (serial)

TUS upload complete → MarkAndReserve + rename → SubmitReserved
```

The `media:created` subscription uses a **separate WS connection** (if the main NimoOS service hasn't been upgraded and the event isn't registered, this subscription gets rejected with 400 and backs off/retries, without dragging down the `media:deleted` connection); event handling is asynchronous via `go` — walking a large directory can take tens of seconds, and running it synchronously would stall the read connection, while MessageBus sends non-blockingly to slow readers and just drops events. The generic subscription loop lives in `runBusPathsSubscriber` in `service/busdelete.go`.

Scan scope is a blocklist rather than an allowlist (`service/scanroots.go` `isUserPartition`): any mount point under `/media` or `/mnt` is scanned by default, but `/media/devmon/<volume label>` (removable USB drives/card readers auto-mounted by devmon) is entirely excluded by product decision — not scanned, not tracked as offline by MountGuard, and any historical devmon assets (including CLIP vectors and thumbnails) are hard-deleted at startup. RAID (`/media/RAID_*`), single-disk storage (`/mnt/Disk-*`), MergerFS, etc. are still included normally. Known limitation: if devmon is disabled/unmounted, the same USB drive may get grabbed by LocalStorage and remounted at `/mnt/Disk-*`, at which point it will be re-scanned as an ordinary fixed disk.

**processFile pipeline (`service/indexer.go`):**

1. Read the file → SHA-256 dedup (skip if `status='indexed'`)
2. MIME detection → determine image/video
3. **Image**: `goexif` parses EXIF (capture time, GPS, camera model, ISO, aperture, shutter, etc.)
4. **Video**: `ffprobe` extracts duration, resolution, codec, frame rate, bitrate, rotation, creation time, GPS; then `ffmpeg` extracts a key frame for downstream ML
5. INSERT/UPDATE `assets` + `asset_exif` (status `'pending'`)
6. Generate thumbnails (small 250px / large 1280px, via `disintegration/imaging`)
7. ML inference (once the ML service is ready):
   - **CLIP image embedding**: `ViT-SO400M-16-SigLIP2-384__webli` (SigLIP2 SO400M, better discrimination than the old nllb-clip-large on short-word/mixed CN-EN queries, see the comment in `common/constants.go`), 1152-dim, computed from the small.jpg thumbnail (matching what the user actually sees), written to `clip_embeddings` (sqlite-vec vec0 virtual table)
   - **Face detection+recognition**: `antelopev2` (InsightFace ResNet100@Glint360K), 512-dim, written to `face_detections`
   - **OCR**: `PP-OCRv5_server`, text lines with confidence ≥0.5 are written to `asset_ocr`, and in the same transaction each line's text + normalized four-corner coordinates are written to `asset_ocr_lines` (delete-then-insert, `boxes_ver=1`; used for search-hit highlighting). Afterward, `computeDocVerdict` computes the mixed criterion for "OCR/document" classification (CLIP zero-shot semantic margin + line geometric regularity; the density-candidate gate always sits outside `hasOcrExpr` at the query layer, `is_doc` only vetoes; weights and calibration are in the five `Doc*` settings in photos.conf, defaults calibrated against a real library on 2026-07-09). **Images only** — videos don't run OCR (OCR on key frames is meaningless and would misclassify screen recordings/frames containing text into "OCR/document"); `pruneVideoOCR` cleans up historical leftover video OCR lines at startup
8. Update status to `'indexed'`

### 2. Semantic search (CLIP)

```
POST /search/smart
  → CLIPTextEmbed(query) → 1152-dim query vector
  → sqlite-vec KNN: "WHERE clip_embeddings MATCH ? AND k = ?"
  → sort by cosine distance
  → (optional) splice OCR exact-match results to the top
  → attach face names + place names
```

Smart View also reuses the `SmartSearch` interface to define dynamic albums by natural-language conditions.

### 3. Face clustering

`FaceService.StartScheduler` clusters periodically/on trigger:
- DBSCAN (`epsilon=0.6, minPoints=1`), clustering 512-dim cosine distances
- Isolated faces are attached to existing person centroids (`assignEpsilon=0.55`)
- Merge-suggestion threshold `suggestEpsilon=0.75`
- Results written to `persons` + `face_person`

**Re-clustering anchors** (`personAnchoredCond`, `service/faces.go`): a person survives re-clustering with its identity (id/name/cover) intact if it's named, favorited, has a `relation`, is hidden, **or has a user-pinned cover (`cover_locked=1`) or hero (`hero_asset_id`)** — the latter two were added so a manually chosen cover/hero doesn't get reshuffled by the next clustering pass. This anchor set only protects a person *while it still has member faces*: the separate zero-faces purge paths (`purgeAutoPersons` / `purgeEmptyAutoPersons`) intentionally use the narrower legacy predicate (name/favorite/relation/hidden only, without `cover_locked`/`hero_asset_id`) — a shell person whose only "anchor" was a pinned cover is still purged once it has no faces left.

When a person's active face count drops to zero (merge, detach, or a re-clustering pass), its `cover_face_id` / `cover_asset_id` / `cover_locked` / `centroid` / `confidence` are cleared (`recomputeOneCentroidTx`, `service/persons.go`, used by merge/detach, also clears `hero_asset_id`; the clustering-rebuild path `recomputePersonStatsTx`, `service/faces.go`, does not clear `hero_asset_id` — a minor asymmetry). Independently, `ClearDanglingCovers` (`service/faces.go`) runs on the same **1-minute** scheduler tick as the empty-person purge and self-heals `persons.cover_face_id` pointers that no longer resolve to a `face_detections` row (e.g. after a cascaded asset delete leaves a stale pointer — `cover_face_id` carries no FK constraint).

**Low-confidence gating** (`photos.MinPersonConfidence`, default `0.5` when absent from config — see Sample configuration): unnamed, unfavorited, relation-less auto-clusters whose `persons.confidence` is below this floor are hidden from `GET /persons`, `GET /persons/:id/relations` (co-appearance), and `GET /persons/merge-suggestions`. Named, favorited, related, or hidden persons are always shown/considered regardless of `confidence`.

#### Person delete semantics (undo)

`DELETE /persons/:id` (default, i.e. without `?purge=true`) is a **server-side undoable delete**, not an immediate hard delete: `PersonService.HidePersonForPurge` (`service/persons.go`) sets `hidden=1` and schedules a hard purge `personPurgeGrace` seconds in the future (constant in `service/persons.go`, currently **30s**) via the persisted `persons.purge_at` column. The same **1-minute** scheduler tick that runs `ClearDanglingCovers`/the empty-person purge (see above) also runs `PersonService.PurgeDuePersons`, which hard-purges (reusing `PurgePerson`: exclude faces, drop bindings, delete the row) every person whose `purge_at` has passed. `POST /persons/:id/restore` (`RestorePerson`) cancels the pending purge — it clears `purge_at` back to `NULL` in the same statement that un-hides the person, so a restore that arrives after the sweep has already fired the purge simply finds no row and returns 404 (a benign race, not an error case to guard against).

Because the schedule lives in a persisted column rather than an in-memory timer, **a page reload or a service restart mid-grace-period no longer silently cancels the pending delete** — the previous client-side implementation used a `setTimeout` in the frontend store, which was lost on any reload or crash, quietly turning an intended delete into a permanent no-op. The sweep picks up any overdue row on its next tick regardless of what happened to the process in between. `?purge=true` is unchanged: it still bypasses the grace period entirely and hard-deletes synchronously in the same request. The response bodies of both `DELETE` and `POST /restore` are unchanged, so this is a behavior-only change from the caller's point of view.

**Explicit hide (distinct from the grace-period delete above)**: `POST /persons/:id/hide` (`PersonService.HidePerson`, `service/persons.go`) sets `hidden=1` with `purge_at` left `NULL` — a durable hide with no scheduled purge, so `PurgeDuePersons`' sweep (which only ever touches `purge_at IS NOT NULL` rows) never picks it up. `GET /persons/hidden` (`ListHiddenPersons`) lists these plainly-hidden persons for a hidden-people management surface: `WHERE hidden=1 AND purge_at IS NOT NULL` rows are excluded on purpose, since a person mid-grace-period is "being deleted," not "hidden" — surfacing it in both places would let a user "restore" a row that's about to be hard-purged out from under them via two different UI affordances with different implied semantics. Both explicit-hide and grace-period-hide persons are un-hidden by the same `POST /persons/:id/restore` (`RestorePerson`), which unconditionally clears `purge_at` back to `NULL` alongside `hidden=0`, so restoring an explicitly-hidden person (which never had a `purge_at` in the first place) is a no-op on that column.

#### Listing order contract (`GET /persons`)

`PersonService.ListPersons`' `ORDER BY` is `(p.name!='' OR p.favorite=1) DESC, cnt DESC, p.rowid` — named or favorited persons are ranked as a block ahead of unnamed auto-clusters, and within each block by descending photo count. This was **not** always true (it used to be plain `cnt DESC`); it changed because at least one real frontend surface reads this list unfiltered — the person-detail merge-target picker (`PhotosPersonDetail.vue`'s `mergeCandidates`, driven by the raw `state.people` array with no client-side re-sort) — where a high-count unnamed garbage cluster could otherwise bury a low-count named person under a wall of "Unnamed" entries in the merge dialog. Two other frontend surfaces read the same list but are unaffected by (and don't depend on) this ordering:
- The People grid's **unnamed** section renders clusters in the backend's returned order as-is (no client-side re-sort) — so for unnamed clusters, "backend order" and "grid order" are the same thing by construction, and this ordering change is what determines it.
- The People grid's **named** section applies its own client-side sort (by name/recency/photo-count-frequency, per the page's sort selector) on top of whatever order the backend returns — so for named persons, the backend order is only ever a starting point that gets fully overridden, and this ordering change is invisible there.
`SearchService.ListPersons` (`service/search.go`, a different function backing the search page's person filter chips, ordered by `rowid`) is untouched — it's a separate query with a separate consumer and wasn't part of this contract.

#### Face clustering parameters (`photos.ClusterEpsilon`)

`clusterEpsilon()` (`service/faces.go`) resolves the DBSCAN cosine-distance threshold from `photos.ClusterEpsilon` (falls back to the legacy `0.6` constant if unset/non-positive). The default was changed from `0.6` to **`0.48`** by this project after an offline percolation-cliff study (`cmd/cluster-analysis`, run read-only against a copy of a real production DB, 4409 faces / 8 named persons + one 2612-face garbage cluster):

| eps | minPoints | resulting max cluster size | garbage-cluster retention |
|---|---|---|---|
| 0.40 – 0.48 | 1 | ~363 – 366 | ~14% |
| **0.50** | 1 | **685** | 26% |
| 0.60 (old default) | 1 | **2612** | 100% (59% of all 4409 faces in one unnamed person) |

`maxSize` is flat across 0.40–0.48, then jumps discontinuously at 0.50 and climbs monotonically back to the full 2612 by 0.60 — a percolation cliff, not a smooth tradeoff. **0.48 sits at the safe edge below the cliff**: it dissolves the mega cluster by 86% (2612→366) at the lowest fragmentation cost among the dissolving options, with named-person purity staying at 1.000.

Two knobs were deliberately **not** changed, because the same study showed they don't help:
- **`minPoints` stays `1`.** Raising it to 2 or 3 gives essentially zero extra dissolution (the garbage blob's average node degree trivially clears minPoints=3 almost everywhere) while permanently breaking sparse 2-face identities that only ever have one mutual neighbor within range — recall drops to a flat 0.812 the moment minPoints≥2. Raising minPoints trades real, low-sample identities for near-zero dissolution gain.
- **No bounding-box size gate on the clustering input.** Filtering out small face crops before DBSCAN was tested at 2%/4%/6% of image min-dimension; even the mildest 2% gate deletes a named person's only face, and the 4% gate only shrinks the residual cluster by 5% while erasing two named persons' faces entirely. A size gate deletes legitimate low-sample persons faster than it dissolves the chain, so it is not used as a dissolution lever.

**Merge-suggestion band**: `suggestEpsilon = 0.75` (upper bound) is unchanged, but the band's lower bound is `clusterEpsilon()` itself, so it now runs **0.48–0.75** (previously 0.60–0.75) — under-clustering at the lower epsilon is recovered by this wider suggestion band rather than by DBSCAN itself.

**Rollback**: explicitly set `photos.ClusterEpsilon = 0.6` in `photos.conf` and restart (or trigger a recluster) to return to the pre-project chaining behavior; do not just delete the key, since a missing key resolves to the new `0.48` default, not the old constant.

**Re-tuning**: if a future library needs different tuning (different camera mix, face count, or identity distribution), re-run the sweep with `cmd/cluster-analysis` against a **read-only copy** of that library's DB (never point it at a live `/DATA/.system_data` path) before changing the default — see `cmd/cluster-analysis/README.md` for the full methodology and flags.

#### Two-pass clustering engine (`photos.ClusterEngine`)

`photos.ClusterEngine` selects which algorithm turns free-floating (not yet anchored to an existing person) face vectors into clusters at the point in `clusterStage`/`rebuildPersonsWithProgress` (`service/faces.go`) that used to always call DBSCAN. It has two values:

- **`"apple"` (default)**: a two-pass pipeline modeled on Apple Photos' approach — cheap, chaining-resistant within a session, then a controlled cross-session merge:
  1. **Moment segmentation** (`SegmentMoments`, `service/cluster_moments.go`): walks each face's owning asset's `taken_at` (falling back to `indexed_at` when `taken_at` is `NULL`, e.g. stripped EXIF) in ascending order and starts a new "moment" whenever the gap to the previous timestamp exceeds `MomentGapMinutes`. This mirrors how a person naturally appears in bursts (a photo session) rather than uniformly across time.
  2. **Pass 1 — conservative greedy within moments** (`GreedyMomentClusters`, `service/cluster_engine.go`): within each moment only, faces whose cosine distance is `<= ClusterTightEps` are unioned (union-find). Single-link chaining is deliberately accepted *inside* a moment — the moment boundary already caps how long a chain can grow, so the classic DBSCAN transitive-chaining failure mode can't runaway here.
  3. **Pass 2 — complete-linkage HAC across moments** (`HACComplete`, `service/cluster_engine.go`): the pass-1 clusters (now typically thousands, not tens of thousands, of faces) are merged bottom-up using **complete linkage** (inter-cluster distance = the *maximum* pairwise member distance, via a Lance-Williams update) until the closest remaining pair exceeds `ClusterMergeEps`. Complete linkage is chosen over single/average linkage specifically because it is the most chaining-resistant of the three: merging two clusters can only ever be blocked, never enabled, by an outlier member, which matches this project's "prefer splits over contamination" stance (see the `ClusterEpsilon`/percolation-cliff history below — this project has a standing bias against re-introducing that failure mode). This is also the two-pass pipeline's regression-tested guarantee: three clusters A/B/C where d(A,B) and d(B,C) are both below `ClusterMergeEps` but d(A,C) is far above it must **not** all merge into one just because B bridges them — complete linkage merges A+B first, and the post-merge distance to C is `max(d(A,C), d(B,C))`, which stays above the stop threshold, leaving C on its own. A single-link/DBSCAN-style pass would chain all three together through B.
  - Only free faces reach this pipeline — an already-anchored face (step 3 of `rebuildPersonsWithProgress`, i.e. snapping a free face onto an existing named/favorited/hidden/cover-locked/hero person's centroid) is never mixed into moment segmentation or either pass, so a free cluster can't get transitively chained onto an anchor purely via an anchored bystander.
- **`"dbscan"`**: the legacy single-pass engine described above (`DBSCANWithProgress`, `epsilon=ClusterEpsilon`, `minPoints=1`), run once over every loaded face (anchored and free alike) exactly as before this switch existed.
- An unrecognized value falls back to `"apple"` with a `zap.Warn` log entry (`clusterStage`), so a config typo is discoverable rather than silently picking undefined behavior.

**Config keys** (`pkg/config/config.go`, accessors in `service/faces.go`: `clusterEngine()`/`momentGap()`/`tightEps()`/`mergeEps()`, all falling back to their documented default when config isn't initialized or the value is absent/non-positive):

| Key | Default | Meaning |
|---|---|---|
| `ClusterEngine` | `apple` | `"apple"` (two-pass) or `"dbscan"` (legacy single-pass) |
| `MomentGapMinutes` | `60` | Time gap (minutes) that starts a new moment in pass 1 |
| `ClusterTightEps` | `0.35` | Pass-1 within-moment union epsilon (cosine distance) |
| `ClusterMergeEps` | `0.55` | Pass-2 HAC stop distance (cosine distance) |

**Rollback**: set `photos.ClusterEngine = dbscan` in `photos.conf` and restart (or trigger a recluster) — this is a pure config flip, no code change, and DBSCAN's own `ClusterEpsilon` knob (see above) is untouched by the apple engine's thresholds, so it resumes exactly where it left off.

**Calibration method and production values**: `cmd/cluster-analysis -mode twopass` (see `cmd/cluster-analysis/README.md` § "Two-pass grid calibration") grid-scans `T_tight ∈ {0.30..0.40}` × `T_merge ∈ {0.45..0.60}` for each `gap ∈ {30, 60, 120}` minutes against a **read-only** copy of a production DB, reporting per-combo cluster count, max cluster size, named-person purity, and named-person fragment count (same purity/fragmentation methodology as the `ClusterEpsilon` percolation-cliff study). The selection rule is the same standard used there: among `purity == 1.0` combos, minimize the named-person fragment count, tie-break on smaller max cluster size (deterministic pessimistic tie-break, see `cmd/cluster-analysis/majority.go`). The 2026-08-20 production run selected **`gap=30min`, `T_tight=0.32`, `T_merge=0.59`**, deployed in `/etc/nimoos/photos.conf` — yielding **1182 clusters**, **max cluster size 235** (vs. 453 for DBSCAN at `ClusterEpsilon=0.48` on the same data), and **named-person purity 1.0**. Re-run the sweep the same way (never against a live `/DATA/.system_data` path) if the library's camera/face-count/identity distribution changes enough to warrant re-tuning.

#### Cluster-merge questions (gray-band merge suggestions, apple engine)

Pass-2's HAC (above) intentionally **stops merging** once the closest remaining pair of clusters exceeds `ClusterMergeEps` — that's the whole point of the stop line: it keeps the engine chaining-resistant. But a pair sitting just *above* that line is often genuinely "almost merged" — the same person, split by a distance the engine was deliberately unwilling to cross on its own. Rather than leaving that fragmentation to only ever be found by scrolling the People grid, `service/merge_questions.go` turns those near-misses into a reviewable queue: **accept merges the two clusters right now** (no naming needed — neither side may even have a name yet); **reject pins a durable cannot-link** so the same pairing doesn't keep resurfacing every pass.

**Generation (`generateMergeSuggestionsTx`, called from `rebuildPersonsWithProgress` step 6, apple engine only, same transaction as steps 1-5)**: after step 5 has written back every person's final centroid/confidence for this pass, this stage builds the pass's full final person set — every freshly-created auto person (step 4, member vectors already in memory from that step) plus every anchored person (`personAnchoredCond`, member vectors re-queried fresh via `loadAnchoredMemberSets` since step 1.5's revalidation may have changed their membership after step 1's own load) — and looks for pairs whose **complete-linkage distance** (same definition `HACComplete` itself uses: max pairwise member distance) falls in `(ClusterMergeEps, ClusterMergeEps+MergeSuggestBand]`.

- **Exclusions**: a pair where *both* sides are named is never suggested (standing rule — naming already answered the "who is this" question, a merge here would need a human decision this queue isn't for). A pair is also suppressed if any `face_negative_pairs` row exists between the two clusters' member faces (a durable answer from a previous rejection).
- **Direction**: `into` (the merge target) is the named side when exactly one side is named, otherwise the larger cluster (by member count); a tie breaks on id for determinism.
- **Pruning**: a centroid-distance prefilter (with a `+0.2` slack over the band ceiling) skips the full `O(|A|×|B|)` complete-linkage computation for most pairs before it's paid. For L2-normalized embeddings this has a **derivable justification**, not just a heuristic feel: writing meanSim/minSim for the mean/min pairwise similarity between two clusters' members, complete-linkage distance is `1-minSim`, and `mean >= min` unconditionally gives `1-meanSim <= 1-minSim`; separately `cosDist(centroidA,centroidB) = 1 - meanSim/(‖centroidA‖·‖centroidB‖)`, and since each centroid norm is `<=1` (a mean of unit vectors), this is `<= 1-meanSim` whenever `meanSim >= 0`. Chained together: whenever the average cross-pair similarity is non-negative — true for every pair actually reachable inside the gray band (per-pair similarity `>= 1-ceiling`, empirically `>= ~0.35`) — `cosDist(centroidA,centroidB) <= complete-linkage distance`, i.e. the prefilter is a genuine lower bound, not just a probably-safe cutoff, making the `0.2` slack roughly 10x more conservative than the bound requires. The one precondition this leans on — L2-normalized face embeddings — is an assumption of the ArcFace-family model feeding this pipeline, **not asserted anywhere in code**; shrinking the slack below its current value should not happen without first adding an explicit normalization check. At production scale (~1200 final persons/pass) this pruning is what keeps the stage in the sub-second range instead of several seconds: `TestGenerateMergeCandidates_PerformanceAt1200ClusterScale` (`service/merge_questions_internal_test.go`) pins a 1200-cluster/24215-face synthetic pass (including one 235-member "mega" cluster, matching the documented production max cluster size above) under 10s — measured well under 1s in practice.
- **Cap**: only the `MergeSuggestCap` closest candidates (dist ascending) are kept per pass.
- **Storage is canonical, not directional**: `merge_suggestions` stores `person_a`/`person_b` with `person_a < person_b` enforced (`orderPair`, same convention as `merge_rejections`/`face_negative_pairs`, plus a DB-level `CHECK`), and a separate `into_is_a` column carries which side is the merge target — resolved at generation time and re-derived at read time (`mergeSuggestionDirection`/`resolveMergeSuggestionDirection`, `service/merge_questions.go`). A **directional** `UNIQUE(from,into)` was the original shape but was reworked before this feature shipped: the larger-cluster-wins-into rule can flip which side is "into" as member counts change across passes, which would let the *same physical pair* collide with itself under the opposite direction instead of ever hitting the uniqueness constraint — canonicalizing the key on the unordered pair closes that edge outright, since there is exactly one row per pair, ever.
- **Persistence**: an `INSERT ... ON CONFLICT(person_a, person_b) DO UPDATE SET dist=excluded.dist, into_is_a=excluded.into_is_a WHERE status='open'` upsert refreshes an open row's distance *and* direction but leaves a decided row completely untouched (same immutable-once-decided invariant as `person_suggestions`). Before inserting, a cleanup `DELETE`s any open row whose `person_a`/`person_b` no longer exists — **auto person ids are rebuilt every pass** (step 2 deletes every non-anchored person before step 4 recreates them from scratch), so without this, stale rows referencing dead ids would accumulate forever.

**Read/decide API (`PersonService.ListMergeQuestions`/`AcceptMergeSuggestion`/`RejectMergeSuggestion`, `service/merge_questions.go`)** — see the API routes table above for the full `GET .../v2` + accept/reject surface. Accept reuses `mergePersonsTx` (factored out of `SearchService.MergePersons` specifically so the merge and the `status='accepted'` write commit atomically together) — same centroid/confidence recompute machinery as the plain merge endpoint. Reject pins a `face_negative_pairs` row between the two clusters' **top-quality representative faces at decision time** (exemplar-first, then score-ordered, same ordering as the preview-face query), normalized `face_a < face_b`: this is what makes the cannot-link survive the next pass's auto-person id churn — even though the two clusters get rebuilt with brand-new ids, if the same two representative faces end up members of two candidate clusters again, generation's negative-pair lookup suppresses the pair before it's ever re-suggested.

**Config keys** (`pkg/config/config.go`, accessors in `service/merge_questions.go`, both falling back to their documented default when config isn't initialized or the value is non-positive):

| Key | Default | Meaning |
|---|---|---|
| `MergeSuggestBand` | `0.06` | Width of the gray band above `ClusterMergeEps` a pair's complete-linkage distance must fall into |
| `MergeSuggestCap` | `30` | Max candidates kept (closest dist first) per clustering pass |

**Distinct from two other, unrelated systems** sharing the "merge suggestion" name: the **legacy** `GET /persons/merge-suggestions` (`PersonService.MergeSuggestions`, `merge_rejections` table) computes named-cluster centroid pairs on the fly on every call, is engine-agnostic, and is left completely untouched by this feature; the **exemplar-assignment** join/review queue (`person_suggestions`, described below) is about individual **faces** joining/leaving a person, not two **clusters** merging.

#### Exemplar templates + KNN assignment (anchored persons, apple engine)

The apple engine described above only covers **free** (not yet anchored) faces. For **anchored** persons (named/favorited/related/hidden/cover-locked/hero — `personAnchoredCond`), the apple engine replaces the old single-centroid + `assignEpsilon` snap with a set of **exemplar templates per person** plus KNN plurality voting. This whole subsystem is `rebuildPersonsWithProgress`'s (`service/faces.go`) step 1 (exemplar selection) → step 1.5 (revalidation) → step 3 (free-face assignment), backed by `service/exemplars.go` (`SelectExemplars`) and `service/matcher.go` (`BuildExemplarIndex`/`Match`). The `dbscan` engine is entirely untouched by any of this (ENGINE SPLIT comments throughout `faces.go`) — a rollback to `dbscan` gets the whole legacy centroid-snap stack back, not a partial mix.

**1. Exemplar selection (`SelectExemplars`, step 1)** — for each anchored person, up to `ExemplarMaxPerPerson` (default 24) of its member faces are chosen as that person's templates: a hard quality gate first (score/frontality/sharpness against `ExemplarMinScore`/`ExemplarMinFrontality`/`ExemplarMinSharpness`, default `0.75`/`0.5`/`0.3` — `NULL` on any signal always fails, so pre-gen4 rows without signals never become exemplars), then `confirmed=1` faces that pass the gate are seeded first (the strongest signal — a real user confirmation), then the remaining slots are filled by farthest-point sampling over cosine distance so the set spans appearance variation (age/glasses/lighting) instead of clustering around one look. A person with zero gated exemplars (e.g. an all-pre-gen4 membership) is skipped entirely by steps 1.5/3 rather than treated as "everyone drifted" — see the code comment at the top of step 1.5.

**2. KNN plurality matcher (`BuildExemplarIndex`/`Match`, steps 1.5 and 3)** — every anchored person's exemplar vectors are flattened into one flat brute-force KNN index (bounded by `cap × persons`, well under 10k on any realistic library). `Match(vec, faceID, negatives, k, minVotes, autoDist, suggestDist)` takes the `k` nearest exemplars overall (`AssignKNNK`, default 5; after dropping any exemplar owned by a person explicitly negated for this face — see `person_negatives` below), and the person holding a **strict** plurality of those `k` wins, provided its vote count clears an **effective floor**: `min(minVotes, that person's own total exemplar count)`, floored at 1 — so a freshly-anchored 1-2-exemplar person isn't starved just for having few templates (`AssignMinVotes`, default 3). The winner's median distance among the `k` (mean of the middle two on an even split) then maps to a decision: `<= AssignAutoDist` (default 0.45) → **auto**-join; `< AssignSuggestDist` (default 0.60) → **suggest** (gray zone); otherwise **none**. A vote-count tie never picks a winner, regardless of distance.

**3. Per-pass revalidation (step 1.5)** — the drift-killer for "members can enter but never leave": every pass, every anchored person's CURRENT members that are `confirmed=0` and not exempt (not the `cover_locked` cover face, not on the person's `hero_asset_id`) are re-matched against **that person's own exemplar set alone** (never the global index — the question is "do you still look like this person", not "does anyone else want you more"). `auto` → membership needs no action, but also **resolves** (deletes) any now-moot OPEN `kind='review'` row for this pair, if one exists from an earlier pass the member has since recovered from (see point 5 below); `suggest` → membership kept, an open `kind='review'` suggestion is queued/refreshed (a decided row is never silently reopened — the UPSERT's `WHERE status='open'` guard); `none` → detached (`DELETE FROM face_person`, **not** a `person_negatives` write — algorithmic auto-eviction isn't a user denial) and returned to the free pool for this same pass. A person that loses a member has its exemplar set recomputed before step 3 reads it, so a just-detached face never lingers as a stale template.

**4. Suggestions queue (`person_suggestions` + `person_negatives`)** — `kind IN ('join','review')`: `join` is a free face proposing to attach to a person (step 3); `review` is an existing member drifting into the gray zone (step 1.5). `status IN ('open','accepted','rejected')`. `GET /persons/suggestions` (`PersonService.ListSuggestions`) groups every open suggestion by visible person, ordered like `ListPersons` (named/favorited first, then photo count desc, ties broken deterministically by `p.rowid`) — see the API routes table above for the full accept/reject/batch surface, and `GET /faces/:id/thumbnail` for rendering a candidate face that may not belong to any person yet. Accepting always wins even if the face auto-joined a *different* person in the meantime (both persons' stats get recomputed); rejecting a `join` records a `person_negatives(person_id, face_id)` row so KNN voting never re-proposes that pair (`Match` drops a negated person's exemplars from the pool for that face entirely, letting a runner-up win in its place); rejecting a `review` also detaches the existing membership first. This accept/reject surface stays live and keeps mutating `face_person`/`confirmed` regardless of `photos.ClusterEngine` — including while set to `dbscan` — which is harmless (the `dbscan` engine's own centroid-snap logic never reads `confirmed` or exemplar state) but worth knowing: switching engines does not pause or gate the suggestion queue.

**5. Resolving moot suggestions on `auto` (final whole-span review fix)** — an `auto` decision, in EITHER step 1.5 or step 3, means the system itself just settled, in the affirmative, the exact question an earlier pass's open suggestion row was asking a human ("does this face belong here"). Both `auto` branches therefore call the shared `resolveOpenSuggestion` (`DELETE FROM person_suggestions WHERE person_id=? AND face_id=? AND status='open'`) for that pair before moving on: a member recovering from the gray zone back into its own person's auto band (step 1.5) clears its stale open `review` row, and a free face auto-joining a person it previously only had an open `join` row for (step 3) clears that row too. Only OPEN rows are ever touched — a DECIDED (`accepted`/`rejected`) row is a real user decision and is left alone as an audit trail, never deleted by this path. Without this, a human could later reject a now-moot card: for a stale `review` row that means detaching an otherwise still-good member AND permanently negating it (no un-negate surface exists); for a stale `join` row it means writing a `person_negatives` row for a face that is simultaneously a confirmed member of that same person, which a later revalidation pass would then silently evict once `Match`'s negation filter strips that person's own exemplars out of the face's pool.

**Config keys** (`pkg/config/config.go`, accessors in `service/persons.go`, all falling back to their documented default when config isn't initialized or the value is non-positive):

| Key | Default | Meaning |
|---|---|---|
| `ExemplarMaxPerPerson` | `24` | Max exemplar templates kept per person |
| `ExemplarMinScore` / `ExemplarMinFrontality` / `ExemplarMinSharpness` | `0.75` / `0.5` / `0.3` | Hard quality-gate floors a face must clear to become/remain an exemplar |
| `AssignKNNK` | `5` | Neighborhood size `k` for `Match` |
| `AssignMinVotes` | `3` | Vote-count floor a person's plurality must clear (see the effective-floor rule above) |
| `AssignAutoDist` | `0.45` | Median-distance upper bound for auto-join/auto-keep |
| `AssignSuggestDist` | `0.60` | Median-distance upper bound for the join/review gray zone |

**Open operator item — these six values are engineering defaults, not production-calibrated**: unlike `ClusterTightEps`/`ClusterMergeEps` above (which went through an offline grid-calibration sweep against a real production DB, see "Two-pass clustering engine"), `AssignAutoDist`/`AssignSuggestDist` and the three `Exemplar*` quality-gate floors have received no equivalent calibration pass — they're reasonable starting points, not measured optima. Re-running a similar `cmd/cluster-analysis`-style sweep against a read-only production DB copy for these six values is a documented follow-up, not yet done.

**Migration behavior (one-shot, `.exemplar_init_v1.done`)**: this exemplar/matcher/revalidation/suggestions stack replaced the old centroid-snap behavior for anchored persons on the same deploy that introduced `face_person.confirmed`. Every existing anchored person's members got to their current membership through the *old* centroid snap, never through a real user confirmation of each individual face — a person's name anchors the **cluster** as a whole, not every member face — so the very first exemplar-engine revalidation pass after this deploy must not silently detach anyone before a human has had a chance to review the gray zone. `FaceService.RunExemplarMigration` (`service/exemplar_migrate.go`, marker-guarded exactly like `BackfillSharpness`'s `.face_sharpness_backfill_v1.done` pattern) is started from `main.go` with no delay (unlike the sharpness backfill's 3-minute wait — this isn't extra work competing with the cold-start burst, it's just an explicit prompt for the clustering pipeline that would run anyway) and is a cheap no-op on every startup once `.exemplar_init_v1.done` exists in `DataPath`. The migration has three conceptual steps:
1. **Set every existing anchored member's `confirmed` to 0.** This step is a documented no-op, not dead code that was forgotten: `face_person.confirmed` is a brand-new column this same feature added (`ALTER TABLE ... DEFAULT 0`), and the only code path anywhere that ever writes `confirmed=1` is `decideSuggestion`'s accept branch — which itself has nothing to accept until this migration's own pass (step 2 below) populates `person_suggestions`. Every pre-existing row is therefore already `confirmed=0` by the `ALTER`'s own default; a bulk `UPDATE` here would touch zero rows by construction.
2. **Run one exemplar-selection + KNN-assignment pass immediately** — `RunExemplarMigration` calls `RunClustering` directly rather than waiting for an incidental future trigger.
3. **That pass's own step-1.5 revalidation runs in "lossless" mode**: a member that would normally be detached (`none`, beyond `AssignSuggestDist`) is instead demoted to an open `review` suggestion — same write path the ordinary gray-zone case already uses, just reached from the `none` branch too. The "first pass" flag is deliberately **not** cached service state (`RunClustering` resets across restarts, so a boolean field on `FaceService` would go stale) — `rebuildPersonsWithProgress` checks the marker file's absence/presence fresh on every call via `exemplarMigrationLosslessPass(markerDir)`, and only writes the marker (`writeExemplarMigrationMarker`) *after* that same call's transaction commits, so a later failure in the same pass can never leave the marker "done" while its lossless demotions never actually persisted. `FaceService.markerDir` (set via `SetMarkerDir`, wired from `cfg.DataPath` in `service.NewService`) empty — as in every test that never calls `SetMarkerDir` — disables migration-awareness entirely, keeping every pre-existing revalidation test's plain-detach expectations intact unmodified.

**Expected post-migration surge**: any anchored person whose membership was assembled loosely under the old centroid snap will surface a burst of open `review` suggestions the first time this runs — most notably a already-existing 453-face test cluster (person id `"1"` in this deployment) is expected to generate many review suggestions once migrated. This is the intended outcome, not a bug: the whole point of the migration is to surface exactly this kind of loosely-attached membership into the human-reviewable queue instead of silently keeping (or silently dropping) it.

#### Threshold self-calibration (`service/calibrate_*.go`)

Five threshold constants that used to be pure engineering guesses — `AssignAutoDist`/`AssignSuggestDist` (KNN exemplar-assignment, see above), `ClusterTightEps`/`ClusterMergeEps`/`MomentGapMinutes` (two-pass apple engine, see above) — now self-calibrate on-device from the same accumulated human decisions `cmd/cluster-analysis`'s `-mode knn`/`-mode merge`/`-mode twopass` offline studies were built to analyze, instead of requiring an operator to run those tools by hand against a DB copy. It's an in-service background process, not a new user-facing feature: no new UI, no data leaves the device, and every rail below exists to keep an unattended adjustment from ever drifting somewhere unsafe.

**Four-layer effective-threshold resolution (`resolveThreshold`, `service/calibrate_resolve.go`)** — every one of the five keys' production accessor (`assignAutoDist`/`assignSuggestDist` in `service/persons.go`, `tightEps`/`mergeEps`/`momentGap` in `service/faces.go`) now resolves through the same four layers, highest-priority first:

1. **`conf` (conf-explicit)** — `config.Cfg.Explicit[key]` is `true` (the config file set this key) and the value is positive. An operator who has hand-tuned a key in `photos.conf` freezes it forever at that value; self-calibration still runs and records what it *would* have done, but a conf-explicit key is skipped before ever reaching `boundAdjust` and its `calibration_state` row (if any, from before the conf line was added) is never read.
2. **`calibrated`** — a `calibration_state` row for the key whose `model_gen` matches the running binary's `common.MLModelGen` exactly. A model-generation bump (new face/CLIP/OCR model combination) invalidates every previously calibrated value at once, without deleting the rows: a stale-gen row simply falls through to layer 3 until self-calibration earns a fresh value under the new generation's own face geometry.
3. **`profile`** — `Thresholds[key].Default` from the stored `CalibrationProfile` (`photos_meta` key `calibration_profile`), or the compiled-in `builtinFactoryProfile()` (version 1) when nothing has been stored yet.
4. **`code`** — the accessor's own hardcoded literal (0.45/0.60/0.35/0.55/60), identical to the pre-calibration fallback constants — the floor nothing can fall through.

Layers 2-4 are cached in-process (`thresholdCache`) until `invalidateThresholdCache()` runs (after every calibration write and every profile update); layer 1 is re-checked uncached on every call since `config.Cfg` can be swapped per-test. With no DB wired (`SetCalibrationDB` never called — the case for the vast majority of `service` package tests), resolution degrades to conf/code only, so every pre-existing threshold-accessor test stays bit-identical.

**Three calibration tiers**, each with its own truth source, evidence bars, and safety margin, run from `maybeCalibrate` (`service/calibrate_run.go`) fired asynchronously after every successful `RunClustering` pass (single-flighted via a `CompareAndSwap` guard so a slow run is never re-entered by the next pass):

| Tier | Keys written | Truth source | Bars (insufficient-data guard) | Extra guard |
|---|---|---|---|---|
| `knn` | `AssignAutoDist`, `AssignSuggestDist` | `face_person.confirmed=1` rows (positives) + `person_negatives` rows (negatives), median KNN distance recomputed via the real `service.Match` (same core `cmd/cluster-analysis -mode knn` uses) | ≥100 positives, ≥20 negatives, ≥5 distinct persons | Skew guard: held (`held_skewed`) if one person accounts for >0.60 of the positives |
| `merge` | `ClusterMergeEps` | `merge_suggestions` decided rows (`status IN ('accepted','rejected')`), same zero-false-accept cut-point method as `cmd/cluster-analysis -mode merge` | ≥30 decided, ≥10 accepted, ≥5 rejected, ≥8 distinct persons | — |
| `twopass` | `ClusterTightEps`, `MomentGapMinutes`, `ClusterMergeEps` | Named persons' member faces (ground truth) + every active face (grid population), same `T_tight × T_merge × gap` purity==1.0 selection as `cmd/cluster-analysis -mode twopass`, bounded by the stored profile's `[Min,Max]` bands rather than the offline tool's fixed sweep range | ≥5 named persons, ≥300 named faces | Face budget: held (`held_insufficient`) if the active-face population exceeds `TwoPassMaxFaces` (20000) |

`ClusterMergeEps` is written by both `merge` and `twopass`; whichever tier runs last within a given `maybeCalibrate` pass wins for that run, but this is safe because both always start `boundAdjust` from the *current* resolved value and step-limit the move, so across successive runs the two tiers can only ever converge on the same value, never fight over it or jump it.

**Throttling (step 1, before any truth is even loaded)**: `knn`/`merge` require both ≥20 newly decided rows since the tier's last `calibration_history` run (any outcome) *and* ≥`Rules.CooldownHours` (default 24h) since the tier's last `outcome='applied'` run — a clamped-only history never blocks on cooldown. `twopass` has no natural decision-queue signal (named-person ground truth is a standing snapshot, not a stream of decisions), so it instead runs on a fixed 30-day calendar cadence, immediate on the very first run. A tier that isn't due skips silently with no history row.

**Bounded adjustment (`boundAdjust`, `service/calibrate_rules.go`)** — every tier's raw recommendation is filtered through the same rails before it can touch `calibration_state`, in this order: (1) clamp the suggestion into the profile's `[Min,Max]` band for that key; (2) limit the move to at most `Rules.MaxStepDist` (0.02, cosine-distance keys) or `Rules.MaxStepMinutes` (15, `MomentGapMinutes`) per run; (3) a safety-clamp re-check into `[Min,Max]` after step-limiting, covering the case where the *current* value already sits outside the band (e.g. a hot-updated profile narrowed it) — the postcondition is that an `applied`/`clamped` outcome always lands in-band; (4) hysteresis: a move smaller than `Rules.MinDeltaDist` (0.01) / `Rules.MinDeltaMinutes` (10) is held (`held_hysteresis`, nothing written) — but only when `current` already sits inside the band; an out-of-band `current` bypasses hysteresis so it snaps back in range in one call. A `NaN`/`±Inf` suggestion is guarded up front and treated as `held_hysteresis` — it can never reach `calibration_state`. After every touched key resolves, the **would-be full five-key effective set** is checked against two cross-key invariants before anything is written: `AssignAutoDist <= AssignSuggestDist - 0.05` and `ClusterTightEps <= ClusterMergeEps - 0.10` (boundary equality legal, float-epsilon tolerant); a violation discards the whole tier's run for that pass (`invariant_violation`, no `calibration_state` write) rather than applying a partial, unsafe combination.

**Persistence**: a tier that actually adjusts at least one key writes `calibration_state` (UPSERT per adjusted key, `model_gen=common.MLModelGen`) and one `calibration_history` row (`outcome` = `applied` or `clamped`, whichever key clamped hardest) inside a single transaction, then calls `invalidateThresholdCache()` after commit. A tier that holds on every key, is insufficient, skewed, or invariant-violating still writes exactly one `calibration_history` row (`held_insufficient`/`held_skewed`/`held_hysteresis`/`invariant_violation`) with identical `old_values`/`new_values`, so the full decision trail is queryable even when nothing changed.

**Factory profile + hot update (`service/calibrate_profile.go`)** — `CalibrationProfile{Version, Thresholds map[key]{Default,Min,Max}, Rules}` is stored as JSON under `photos_meta.calibration_profile`; `builtinFactoryProfile()` (version 1) is the compiled-in fallback whenever nothing is stored or the stored row is corrupt (logged, never fatal). `storeCalibrationProfile` enforces: valid JSON; strictly increasing `Version`; all five calibratable keys present with `Min<=Default<=Max` and `Min>0`; every `Rules` field `>0`. A successful `PUT` takes effect immediately — `invalidateThresholdCache()` runs before the response, so the very next `GET /persons/calibration` (or the next clustering pass) observes it without a restart.

**API routes** (registered ahead of `GET /persons/:id` in `route/router.go`, same route-order trap as `/persons/hidden`/`/persons/suggestions`; none are in the JWT-exempt list — this surface can move every self-calibration safety rail):

| Method | Path | Purpose |
|---|---|---|
| GET | `/persons/calibration` | Each of the five keys' effective value + resolution source (`conf`/`calibrated`/`profile`/`code`) + model gen; the stored profile's version; each tier's evidence-bar progress (have/need) and last-run time/outcome |
| GET | `/persons/calibration/history?limit=` | Up to `limit` (default 50, capped 500) `calibration_history` rows, newest first: tier, outcome, truth counts, old/new values per key |
| PUT | `/persons/calibration/profile` | Replace the stored `CalibrationProfile` (validated, strictly-increasing version); serialized against concurrent writers via a package-level mutex |

**Operational notes**: setting any of the five keys explicitly in `photos.conf` permanently overrides self-calibration for that key (layer 1 always wins) — the deploy migration below relies on this to hand control to self-calibration by *removing* the conf lines, not by adding new ones. A model-generation bump (`common.MLModelGen`) silently invalidates every calibrated value without deleting `calibration_state` rows; they simply stop matching and layer 3 (profile) takes over until fresh evidence re-earns a calibrated value under the new generation. The whole runner is deliberately not mutually exclusive with a concurrent clustering pass — it reads its own WAL snapshot, writes in one small transaction, and any values it applies are only ever picked up by `resolveThreshold`'s cache on the *next* clustering pass, so there is no feedback loop into the pass that triggered it.

#### Performance notes

- `face_person(person_id)` is indexed (`idx_face_person_person`, `pkg/sqlite/db.go`), backing the person-scoped face lookups used throughout re-clustering and the `/persons/:id/*` endpoints.
- DBSCAN's neighbor-list computation is parallelized across CPU cores (`DBSCANWithProgress`, used by the production clustering pipeline) instead of the old serial `regionQuery` loop. Benchmarked on a 4409-face production library (16 cores): serial 6.65s → parallel 0.61s, **~11x speedup**, with output labels identical to the serial path (verified by `cmd/cluster-analysis`).
- The apple engine's `HACComplete` (pass 2) has its distance matrix sized by *pass-1 cluster count*, not raw face count, so it stays cheap even at production scale: `TestHACCompletePerformanceSmokeAt5095Scale` (`service/cluster_engine_test.go`) pins 5095 faces / 1800 pass-1 clusters under 10s, and `TestHACCompleteWorstCasePerformanceAt3000SingletonClusters` pins the pathological worst case (3000 singleton clusters, every pair merges into one) under 20s — both comfortably inside this project's 30s full-pipeline clustering budget (see the plan's Global Constraints).

#### Known behavior: residual cluster after re-clustering

After adopting `ClusterEpsilon=0.48`, re-clustering a library with a pre-existing garbage mega-cluster leaves one **residual unnamed cluster** rather than dissolving it to nothing — in the validation run above, 366 faces with `ClusterConfidence = 0.8613`. This is above the `MinPersonConfidence` gate (default `0.5`, see above), so it **will still appear** in `GET /persons` as one unnamed person, not be filtered out. This is expected and orthogonal to the epsilon fix: `ClusterConfidence` measures members' average similarity to their own cluster's centroid, which stays high even for an imperfect multi-identity fragment as long as its members are still mutually closer than the epsilon threshold — the confidence gate catches low-cohesion garbage, not merely-imperfect (but still tightly-clustered) ones. Reducing epsilon further would re-fragment named, sparsely-sampled identities before it fully dissolves this residual, so it is left as-is; within-cluster purity metrics (splitting a "confident but still-mixed" cluster) are a documented follow-up, not solved by this project.

### 4. Embedder backfill

`Embedder.Run` checks ML readiness every 30 seconds:
- On a **false → true** transition, triggers asynchronously:
  1. `Backfill`: backfill embeddings for all assets with `status='indexed'` but missing a CLIP vector
  2. `reembedThumbnailsOnce`: one-time recomputation of all existing embeddings from thumbnails (marker file `.clip_reembed_thumb_v1.done` prevents repeating this)
  3. `BackfillOCR`: backfill for all assets missing OCR text

### 5. Reverse geocoding

`GeoService` reads GPS coordinates from `asset_exif` and reverse-geocodes them offline using an embedded gazetteer (`pkg/geo/data/*.tsv.gz`: 15,000+ cities, countries, POIs), writing to `asset_geo`. When the gazetteer version (`geoGazVersion`) changes, `asset_geo` is automatically cleared and rerun.

### 6. Automatic rebuild on ML model generation change

`common.MLModelGen` (currently `"4"`; gen 2 was the nllb-clip-large era, gen 3 introduced the still-current SigLIP2 SO400M + antelopev2 + PP-OCRv5_server combination) identifies the model combination (CLIP/face/OCR selection + dimensions) bound to the current binary; it's written to `photos_meta.ml_model_gen` after a successful rebuild. At startup, if this key is missing (old DB) or doesn't match the current `MLModelGen`, `Rebuilder.MaybeAutoRebuild` (`service/rebuild.go`) polls until the ML backend is ready (new model cache in place) and then automatically triggers a full rebuild: reruns CLIP/face/OCR on all `status='indexed'` assets, re-clusters faces and cleans up faceless empty `persons`, and finally writes back the new generation. An empty library (no assets) skips the worker pool, goes straight to re-clustering and writing the generation, completing in seconds. The generation is only written after the rebuild finishes successfully (`finalize()`); a mid-rebuild failure or power loss will retry on the next startup.

**Gen 4 — full face regeneration, no model change**: gen 4 bumps `MLModelGen` without touching the CLIP/face/OCR models themselves; its only purpose is to force every asset through a fresh face-detection pass so every `face_detections` row ends up carrying a detector `score`/`frontality`/`sharpness` (older rows predate those columns, or — for `sharpness` only — got them from the one-shot backfill described in Core flows §8, not from a real re-detection), then re-cluster at the current `photos.ClusterEpsilon` default (`0.48`, see "Face clustering parameters" above) and re-rank every person's cover through the now-unified quality-weighted `selectCoverFace` path (see "Five cover-selection sites" and "Cover-selection unification" below). Because a from-scratch re-cluster has no way to reattach a name to whichever new cluster its member faces land in, this rebuild **intentionally drops every existing person, including user-named ones** — a deliberate, authorized product decision for this project, not an accidental regression; anyone with named people will need to re-name them once the rebuild completes (`finalize()`'s `DELETE FROM persons WHERE id NOT IN (...)` prunes exactly the persons a from-scratch re-cluster left with no faces, which after gen 4's full regeneration is effectively all of them). Like every generation bump, the rebuild itself needs no manual trigger — `MaybeAutoRebuild` fires it automatically on the first startup of a binary built with `MLModelGen="4"`. **Known benign side effect**: `clip_text_cache` is keyed by `(key, gen)` (see Data storage), so the gen bump alone — even though the CLIP model is unchanged — misses every cached doc-classification zero-shot prompt vector under the new `gen='4'` key; `loadPromptVecs` (`service/docverdict.go`) transparently re-embeds each prompt (a handful of cheap text-tower calls) and repopulates the cache, with no user-visible effect beyond that one-time re-embed.

### 7. Aesthetic scoring

`pkg/aesthetic` is a pure-Go linear head: it runs a small MLP on top of the existing CLIP (SigLIP2) image vector (`NAES` weight format, embedded into the binary via `go:embed`, see `pkg/aesthetic/weights/head_v1.bin`); the current probe head's version string is `v25probe1`, with dimension chain 1152→1024→128→64→16→1. `Score` first L2-normalizes the input, then applies `y=Wx+b` layer by layer; if the input vector's dimension doesn't match the head, it returns `NaN` (the caller skips that asset rather than writing a bad score). **Probe status**: this head is trained on SigLIP v1, a different vector space from the SigLIP2 vectors actually used here, so scoring quality is not guaranteed — if manual acceptance (via `scripts/aesthetic/report.py`'s top/bottom score comparison page) fails, stage two will train our own linear head on the AVA dataset aligned to the SigLIP2 space, reusing the same NAES format and `Load`/`LoadFrom` interface — only `head_v1.bin` changes, no Go code changes needed (see `scripts/aesthetic/README.md`, conversion script `convert_v25.py`).

Scoring runs on two complementary paths:
1. **Inline**: after `writeClipEmbedding` (`service/indexer.go`) successfully writes a CLIP vector, if `aestheticHead` is non-nil (injected via `AestheticEnabled`), the score is computed on the spot and written to `assets.aesthetic_score` — a pure local matrix multiply, microsecond-scale, no extra ML call.
2. **Backfill**: `Embedder.BackfillAesthetic` (`service/embedder.go`, CAS re-entry guard + rerun-pending semantics same as `BackfillOCR`) scans assets that "have a CLIP vector but `aesthetic_score IS NULL`" and computes scores, registering a task with `type="aesthetic"`. Triggered from three places: service startup (when `AestheticEnabled` is on, this is a pure local computation that doesn't wait for ML readiness — the key difference from OCR backfill), at the tail of the ML-recovery chain (offline→recovered transition), and on completion of every upload batch (`SetOnBatchDone`). **Doesn't depend on ML being online** (only reads CLIP vectors already in the DB, doesn't touch original files), and **doesn't filter out offline** assets.

`assets.aesthetic_score` (REAL, NULL = not yet scored) and `photos_meta.aesthetic_head_ver` manage versioning independently of `ml_model_gen`: `EnsureAestheticHeadVer` (`service/embedder.go`) sets scores to NULL for the whole library and stamps the new version in the same transaction when the head version changes — unlike `ml_model_gen`'s "stamp after success," setting to NULL is itself an atomic clear with no dirty-data window, so the version can be stamped up front, and rescoring naturally converges via `BackfillAesthetic`'s NULL query. The ML model generation rebuild (`service/rebuild.go`, see previous section) clears scores per-asset when vectors change (`UPDATE assets SET aesthetic_score=NULL`), and `ForceReprocess` rewriting vectors gets scores refilled automatically by inline scoring, with no separate task needed.

**Five cover-selection sites** (implicit ranking when not manually specified; `aesthetic_score IS NULL` always sorts last, falling back to the previous ranking rule):
- Album implicit cover (`service/album.go`: both the album-list summary query and the single-album detail query take the highest-scoring member in the album by `aesthetic_score DESC`, with `position`/`rowid` as a stable fallback)
- Place city card + spot cover candidates (`service/places.go`: city-aggregation card and spot best-photo queries)
- Smart View preview seeds (`service/smartview.go`: preview sampling sorted by score)
- Person cover hybrid score (`service/persons.go` `hybridCoverScore`: whole-image aesthetic score × that face's bbox area ratio × a `quality` factor computed by `faceQualityFactor` from the face detection's own confidence (`face_detections.score`) and, when present, its `frontality`/`sharpness` signals (see "Face quality signals" below) — each `NULL` component is treated as neutral, i.e. contributes a factor of `1`; the aesthetic/area factors must be comparable — an unscored asset/missing EXIF width-height/degenerate bbox is marked incomparable; unaffected when `cover_locked=1`, which still skips automatic recomputation)
- Person hero fallback (`service/persons.go`: list/detail hero query when there's no locked cover)

A manually specified cover (`cover_asset_id`/`cover_face_id`/`cover_locked=1`) always takes priority over the aesthetic score; when the whole-library score is NULL (e.g. during a rescore right after switching heads), all five sites above fall back to their own previous ranking (time/position, etc.).

**Cover-selection unification (`selectCoverFace`, `service/persons.go`) — a previously-documented asymmetry is now resolved**: the hybrid-score ranking above was originally wired into only one of the two code paths that recompute a person's cover — `recomputeOneCentroidTx` (the merge/detach/unlock path). The other path, `recomputePersonStatsTx` (`service/faces.go`, the periodic/triggered re-clustering pass), independently picked whichever member face landed nearest the new cluster centroid and never looked at aesthetic score, `frontality`, or `sharpness` at all, breaking centroid-distance ties by plain list order. Net effect: the quality signals described above — including the one-shot `sharpness` backfill — could improve a cover chosen by a merge/detach, but the very next re-clustering pass would silently overwrite it with a lower-quality, nearest-centroid pick that ignored those same signals. Both paths now call the same extracted `selectCoverFace(vecs, centroid, bboxes, aesScores, ws, hs, detScores, fronts, sharps)` function, so periodic re-clustering ranks covers identically to merge/detach/unlock; `recomputePersonStatsTx`'s member-loading query was extended with the same `fd.bbox/score/frontality/sharpness`, `a.aesthetic_score`, and `asset_exif.width/height` columns `recomputeOneCentroidTx` already read, and falls back to the old nearest-centroid pick only when every member face is incomparable (no aesthetic score / no EXIF dimensions) — same fallback both paths always had. `cover_locked` semantics are unchanged in either path.

Config toggle `AestheticEnabled` (`photos.conf`, default `true`), **not hot-reloaded**: turning it off only stops new scoring (inline is skipped, `BackfillAesthetic` isn't triggered); existing scores aren't cleared. Turning it back on requires restarting the service to load the head.

### 8. Face quality signals (frontality + sharpness)

`mlserver`'s face pipeline (`mlserver/server/facemodel.py`) emits two additional per-face floats in `[0,1]` alongside the existing detector `score`, both heuristic rather than learned models:
- **`frontality`** (`frontality_from_kps`): a symmetry heuristic from the 5-point landmarks (left eye, right eye, nose, mouth-left, mouth-right) — a frontal face has its nose x-coordinate near the eye midpoint. Returns `1.0` for a perfectly frontal face, approaching `0.0` for a strong profile; degenerate/coincident eye landmarks (eye distance ~0) also return `0.0`.
- **`sharpness`** (`sharpness_from_crop`): a blur measure on the 112×112 aligned face crop — variance of the Laplacian on grayscale, squashed to `[0,1]` via `v/(v+SHARPNESS_K)` with `SHARPNESS_K=100.0` (the calibration point: `var==100` maps to `0.5`; chosen so a typically sharp face, `var≥~300`, scores `≥0.75`, and a heavily blurred one, `var≤~30`, scores `≤0.23` — re-tune this constant if that calibration drifts on a different camera/lens mix).

The Go client (`pkg/mlclient`) parses both as `*float64` (`FaceResult.Frontality`/`.Sharpness`) — a pointer, not a plain float, so a response that simply omits the fields decodes to `nil` rather than erroring or zero-filling. They're persisted as nullable `face_detections.frontality`/`face_detections.sharpness` (`REAL`, `pkg/sqlite/db.go`), written by `insertFaceDetections` (`service/indexer.go`) straight from the pointer (a `nil` pointer becomes SQL `NULL` automatically via `database/sql`).

**Cover-ranking consumption** (`faceQualityFactor`, `service/persons.go`, feeds the `hybridCoverScore` `quality` parameter — see "Five cover-selection sites" above): `detScore` keeps the pre-existing raw-clamp semantics (`NULL`→`1.0` neutral, otherwise `clamp01`); each of `frontality`/`sharpness`, when non-`NULL`, multiplies in a range-compressed factor `0.5 + 0.5*clamp01(signal)` — so a *present* weak signal (e.g. a full profile shot, `frontality=0`) still only halves the score rather than annihilating it, while an *absent* (`NULL`) signal contributes a neutral `1.0` and is skipped entirely.

**NULL-neutral / rollback-compatibility contract** (the reason this shipped as three independent, individually-safe layers): if the ML backend in front of `mlclient` doesn't emit these fields at all — e.g. a rollback to the old immich-ml container, which has no concept of them — the JSON response simply lacks the keys, `mlclient` unmarshals `Frontality`/`Sharpness` as `nil`, `insertFaceDetections` writes SQL `NULL`, and `faceQualityFactor` treats both `NULL` inputs as neutral (`1.0`, no multiplication). The net effect: **cover ranking is bit-for-bit unaffected** by rolling back to a backend that predates this feature — no error, no crash, no degraded ranking, just a quality factor of exactly the same value it would've been before frontality/sharpness existed at all.

**Legacy data — sharpness has a one-shot backfill, frontality doesn't**: rows written before this feature (or by a rolled-back backend) start with both `frontality`/`sharpness` `NULL`. `frontality` stays `NULL` forever for those rows — it needs the 5-point landmarks, which were never stored, so recovering it would require re-running full ML detection (rebuilding the face rows entirely), a much bigger and riskier operation than this feature warrants; tracked as an optional future ops item, not implemented. `sharpness`, by contrast, only needs the same bbox crop already stored via `face_detections.bbox` plus the same source image `detectFaceScanTarget` used at detection time — no ML round-trip — so it gets a **one-shot pure-Go backfill**: `FaceService.BackfillSharpness` (`service/quality_backfill.go`), started from `main.go` `SharpnessBackfillStartupDelay` (3 minutes) after process startup so it doesn't compete with the cold-start indexing/detection burst, and guarded by a marker file (`.face_sharpness_backfill_v1.done`, mirroring `.clip_reembed_thumb_v1.done`'s one-shot pattern) so a restart never re-runs it. It snapshots every `face_detections` row with `sharpness IS NULL` (joined to a live, non-offline, non-deleted asset) up front into memory, then walks the fixed snapshot in batches of 50 with a 200ms pause between batches — a **fixed snapshot, not a re-query loop**, specifically because unreadable/undecodable sources are skipped **permanently** (never retried): if each batch re-queried `WHERE sharpness IS NULL`, permanently-skipped rows would still match that filter forever and the loop would never terminate. For each row it resolves the same source image the ML detector would have scanned (`resolveFaceScanSource`, shared with `detectFaceScanTarget` so bbox pixel coordinates line up 1:1 with no re-scaling), crops the stored bbox, resizes to 112×112 (matching mlserver's aligned-crop size, though this plain bbox crop isn't landmark-aligned like mlserver's — good enough for relative ranking, not bit-identical), and computes a Laplacian-variance sharpness score on **8-bit-per-channel luma** (`grayLaplacianVariance` right-shifts `image.Image.At().RGBA()`'s native 16-bit components by 8 before applying luma weights — skipping this descale was caught in review: it inflates variance by ~257²≈66049×, saturating nearly every real crop to ~1.0 and destroying the signal), squashed via the same `v/(v+K)` formula with the same `K=100.0` as mlserver's `SHARPNESS_K` so legacy-backfilled and freshly-detected scores land on one comparable scale. The marker is written once at the end regardless of how many rows were skipped, and a skipped-count is logged. `frontality` is untouched by this pass — only `sharpness` is written. New photos indexed after upgrading benefit progressively (detected fresh by the updated `mlserver`, with both signals); the library's cover-selection quality improves gradually for `frontality`, and in one step (a few minutes after the first restart post-deploy) for `sharpness`.

**Deploy note — hard rule**: these two fields only start populating after `mlserver`'s container image is rebuilt from the updated `mlserver/` source (see "Packaging" below) and the container is recreated. **A rebuild/recreate that drops the `docker-compose.override.yml` device-passthrough override silently loses the Intel iGPU and falls back to CPU-only inference** (log symptom: `[OpenVINO] Device GPU is not available`, OCR throughput dropping ~4x) — this is not specific to this feature, it's a standing operational red line for *any* `mlserver` code change that requires a rebuild. Always recreate with both compose files applied (`-f docker-compose.yml -f docker-compose.override.yml`, or `cd` into the app's compose directory and let default file resolution pick both up — never pass `-f docker-compose.yml` alone when an override is present) and verify afterward with `docker inspect --format '{{range .HostConfig.Devices}}{{.PathOnHost}}{{end}}' <container>` — it must print `/dev/dri`.

---

## Data storage

```
/etc/nimoos/photos.conf          Config (INI, read via Viper)
/DATA/.system_data/photos/       DataPath (default, relocatable; all derived data directories below follow it)
  ├── photos.db                  SQLite database (WAL mode)
  ├── thumbs/<asset_id>/
  │     ├── small.jpg            250px thumbnail (CLIP embedding source)
  │     ├── large.jpg            1280px thumbnail
  │     ├── sprite.jpg           Video hover-preview sprite (a few hundred KB), asynchronously pre-generated on ingest; pre-generation is always on, unaffected by any toggle
  │     └── preview.mp4          Low-bitrate video hover preview (can reach tens of MB each); lazily generated by default (generated on first GET /preview); when `photos.PreviewPregen=true`, pre-generated alongside ingest/backfill instead
  ├── face-thumbs/<face_id>.jpg  Face thumbnails; orphans (files outside the `face_detections` set) are reclaimed by Prune, no separate reclaim path
  ├── live/                      Live Photo video segment cache
  ├── ml-cache/                  mlserver model cache (bind-mounted into the container as `/cache`; the container-side mount path is baked into `.env`'s `NIMOOS_PHOTOS_ML_CACHE` by `deploy/ml/install.sh`, automatically following DataPath if it's moved), laid out as `clip/<model>/{visual,textual}`, `facial-recognition/<model>/{detection,recognition}`, `ocr/<model>/{detection,recognition}` (each a `model.onnx` + config), plus `ov-cache/` — the OpenVINO execution provider's compiled-kernel cache, persisted across container restarts/upgrades so the one-time EP compilation isn't repeated on every start
  └── tus-staging/               TUS upload staging (0700 permissions; auto-cleaned after 7 days, see "Staging directory")
/DATA/.system_data/photos-tus-staging/   Legacy fixed staging directory (deprecated), swept only once at service startup as a fallback
/DATA/Gallery/                   Default photo home directory (configurable)
/var/run/nimoos/photos.url       Service-discovery address
/var/log/nimoos/                 Logs (zap)
```

**Daily cache cleanup**: `main.go` starts a 24-hour ticker that calls `StorageService.Prune` (same implementation as the manual `POST /cache/prune` button on the settings page), cleaning up three kinds of orphaned/expired data in one pass: thumbnail directories under `thumbs/` whose asset no longer exists, avatar files under `face-thumbs/` whose `face_detections` row no longer exists, and staging files under `tus-staging/` older than 7 days (`common.StagingMaxAge`). Previously this could only be triggered manually; it's now run automatically every day. There's also a one-time sweep at startup (scanning both the new and legacy staging directories).

### Main SQLite tables

| Table | Purpose |
|---|---|
| `assets` | Main asset table (path, MIME, capture time, checksum, status, soft delete, `aesthetic_score` aesthetic score REAL/NULL=not yet scored) |
| `asset_exif` | EXIF/video metadata (resolution, GPS, camera, ISO, codec, etc.) |
| `clip_embeddings` | sqlite-vec **vec0** virtual table, 1152-dim CLIP vectors |
| `asset_clip_idx` | rowid ↔ asset_id mapping (joins clip_embeddings and assets) |
| `face_detections` | Face detection results (bbox, 512-dim embedding, excluded flag, `score` REAL = detector confidence; `frontality`/`sharpness` REAL = optional per-face quality signals from `mlserver`, see Core flows §8 — `NULL` on legacy rows predating the columns, or on any backend that doesn't emit them, = neutral factor in cover selection) |
| `persons` | Face clustering results (name, cover, centroid, confidence; `hidden` flag drives the `PersonVisible` 404 guard; `cover_locked`/`hero_asset_id` anchor a pinned cover/hero through re-clustering; `purge_at` schedules the undo-window hard purge for a soft-deleted person — see Core flows §3 "Person delete semantics") |
| `face_person` | Face → person mapping (`exemplar`=1 marks one of the person's quality-gated exemplar templates; `confirmed`=1 marks a face the user has explicitly confirmed as belonging, exempt from revalidation forever — both default 0, see Core flows §3 "Exemplar templates + KNN assignment") |
| `person_suggestions` | Open/decided join (`kind='join'`, a free face proposing to attach) or review (`kind='review'`, an existing member drifting into the gray zone) suggestions; `UNIQUE(person_id, face_id)`, `status IN ('open','accepted','rejected')` |
| `person_negatives` | `(person_id, face_id)` pairs a user has explicitly rejected — excluded from `Match`'s exemplar pool for that face forever, so KNN voting never re-proposes the same pair |
| `asset_ocr` | OCR text (coverage, line_count density-candidate gate; boxes_ver=0 means per-line coordinates aren't stored; doc_sem/doc_geo/is_doc are the semantic margin/geometric regularity/final verdict of the mixed criterion (NULL=not yet computed, query falls back to pure density), doc_ver=0 means pending computation; the single write path is computeDocVerdict()) |
| `asset_ocr_lines` | OCR per-line text + normalized four-corner coordinates (JSON, 8 floats, [0,1]); line_no matches the concatenation order in asset_ocr.text; single write path is ocrAsset(); cascade-deletes with assets via foreign key; used for GET /assets/:id/ocr search-hit highlighting and doc geometric-regularity computation |
| `clip_text_cache` | CLIP text-prompt vector cache (key=prompt, gen=MLModelGen); used for the doc-classification zero-shot criterion; auto-invalidated and re-embedded on a model generation change |
| `asset_geo` | Reverse-geocoded location info (city, country, geonameid) |
| `albums` + `album_assets` | Manual albums (supports ordering) |
| `asset_favorites` | Per-user favorites |
| `asset_views` | Per-user view counts |
| `smart_views` + `smart_view_matches` | Semantic auto-albums and their match results |
| `merge_rejections` | Rejected face-merge suggestion pairs (legacy on-the-fly `GET /persons/merge-suggestions`, person-id-keyed) |
| `merge_suggestions` | Cluster-merge questions: durable gray-band candidates generated during `RunClustering` (apple engine); canonical pair `person_a < person_b` (`UNIQUE(person_a, person_b)`, `CHECK(person_a < person_b)`) with direction carried separately by `into_is_a`, `status IN ('open','accepted','rejected')` — see Core flows §3 "Cluster-merge questions" |
| `face_negative_pairs` | Durable face-level cannot-link pairs, `(face_a, face_b)` composite PK with `face_a < face_b` enforced in code — written by rejecting a cluster-merge question, survives auto-person id churn across passes |
| `place_cover_overrides` | User-customized place covers |
| `spot_name_overrides` | User-customized spot names |
| `photos_meta` | Key-value metadata (e.g. `index_last_rebuilt`, `ml_model_gen`, `aesthetic_head_ver`) |

**Timeline/scale indexes** (`pkg/sqlite/db.go`, migration-time): two partial indexes back the timeline endpoints and the general asset list —
- `idx_assets_sortkey`: on `COALESCE(taken_at, indexed_at) DESC`, `WHERE is_live_photo_video = 0 AND deleted_at IS NULL AND offline = 0` — lets `ListAssets`'s default sort walk the index instead of a `TEMP B-TREE` sort.
- `idx_assets_monthkey`: on `strftime('%Y-%m', COALESCE(taken_at, indexed_at))`, same `WHERE` — backs both `/timeline/buckets` (index scan) and `/timeline/bucket` (equality seek).

Both are **partial indexes with SQLite's exact-predicate matching**: any query that reads/writes the same `WHERE`/expression must reproduce the predicate text verbatim, or the planner won't pick the index. The connection pool is also capped at 8 (`SetMaxOpenConns`/`SetMaxIdleConns`), sized for a single-node SQLite-WAL deployment rather than left at the driver default.

SQLite's cost model needs `sqlite_stat1` statistics to route around a low-selectivity equality index (`is_live_photo_video=0`, true for nearly every row) and pick these sort/month indexes instead — without stats it can pick the wrong index and add a `TEMP B-TREE` sort even though a partial index that avoids it exists. `migrate()` therefore does:
1. A **guarded, one-time** `ANALYZE assets` — only runs when `sqlite_stat1` has no rows yet for the `assets` table (fresh DB, or upgrading from a version that never ran it). On a library with ~500k rows this is a single-table scan taking on the order of seconds, and it only happens once across the service's lifetime (not on every restart).
2. An unconditional `PRAGMA optimize` on every subsequent startup — self-limiting (SQLite decides internally whether any table's stats are stale enough to be worth refreshing), near-zero cost on a normal restart with no big data-volume swing.

---

## CGO dependencies

| Dependency | Purpose |
|---|---|
| `mattn/go-sqlite3` | CGO SQLite3 driver, requires system `gcc` + `sqlite3.h` |
| `asg017/sqlite-vec-go-bindings/cgo` | sqlite-vec extension (`vec0` virtual table), auto-registered via `init()` |

Building requires `CGO_ENABLED=1`, with `gcc` and `libsqlite3-dev` (Debian/Ubuntu) or the equivalent package installed on the system.

```bash
CGO_ENABLED=1 go build -o nimoos-photos .
```

---

## Authentication

JWT verification (ECDSA P-256, public key read from `/var/run/nimoos/`), with the following paths **exempt**:
- OPTIONS requests (CORS preflight, sent by TUS clients)
- `/healthz` — genuinely auth-exempt in the Skipper (not just in the handler's comment): the route registration always carried a "no auth required" comment, but the Skipper originally only exempted `/version`, so `/healthz` actually 401'd in production, contradicting its own comment. Fixed by adding an explicit `p == "/healthz"` branch alongside the `/version` check
- `*/thumbnail`, `*/face-thumbnail`, `*/original`, `*/live`, `*/sprite` (media files — an `<img>` tag can't attach an Authorization header)
- `*/favorites/export` (same reason)
- **MCP read-only exemption** (`mcpReadSkip`, `route/router.go`): the two read-only endpoints `POST /search/smart` and `GET /albums`, for internal calls from NimoOS-AI (agent / MCP server). **Fail-closed + exact match**: RealIP must be `127.*` (Gateway strips any forged XFF), the `X-NimoOS-User-ID` header must be non-empty (if missing, no exemption → falls through to JWT → 401, never falls back to a default user), and the path must exactly match the full route (not HasSuffix)

Once verification passes, the user ID from the JWT claims is injected into downstream handling via the `X-NimoOS-User-ID` header.

---

## Sample configuration (`/etc/nimoos/photos.conf`)

```ini
[common]
RuntimePath = /var/run/nimoos
LogPath = /var/log/nimoos

[photos]
DataPath = /DATA/.system_data/photos
MLEndpoint = http://127.0.0.1:3003
Workers = 3
WatchDirs = /DATA/Gallery,/DATA/Documents,/DATA/Downloads
FacesEnabled = true
ScenesEnabled = true
OCREnabled = true
SmartViewEnabled = true
AestheticEnabled = true
MinPersonConfidence = 0.5
PreviewPregen = false
```

- `WatchDirs`: comma-separated fsnotify watch directories, three by default; can be hot-applied at runtime via `PUT /v1/photos/config` (`Watcher.Restart`).
- `Workers`: number of Indexer concurrent workers, default 3.
- `MLEndpoint`: nimoos-photos-ml-server address, default `http://127.0.0.1:3003`.
- Feature toggles (`FacesEnabled/ScenesEnabled/OCREnabled/SmartViewEnabled/AestheticEnabled`) all default to `true`; turning off `ScenesEnabled` means new photos no longer get CLIP vectors, disabling semantic search. `AestheticEnabled` is not hot-reloaded — turning it off only stops new scoring (both inline and backfill are skipped), existing `aesthetic_score` values aren't cleared; see "Core flows § 7 Aesthetic scoring".
- `PreviewPregen`: default `false`, video `preview.mp4` is purely lazy-generated (generated on first `GET /preview`), with pre-generation skipped only during ingest/startup backfill. When set to `true`, it's asynchronously pre-generated on ingest just like `sprite.jpg`, and also covered by `BackfillSprites` for existing videos. `sprite.jpg` is unaffected by this toggle and is always pre-generated.
- `MinPersonConfidence`: default `0.5` when absent from the config file. Cohesion floor for exposing **unnamed, unfavorited, relation-less** auto face-clusters through `GET /persons`, `GET /persons/:id/relations`, and `GET /persons/merge-suggestions`; named/favorited/related/hidden persons are always shown regardless of `confidence`. See Core flows §3 (Face clustering).

---

## Dependencies on other services

| Service | Relationship |
|---|---|
| **NimoOS-Gateway** | Registers three prefixes (`/v1/photos`, `/doc/v1/photos`, `/v1/upload-tus`) via `POST /v1/gateway/routes` at startup; reads the Gateway address from `RuntimePath` |
| **NimoOS-UserService** | Reads the ECDSA public key from `/var/run/nimoos/` to verify JWTs |
| **NimoOS-MessageBus** | systemd `After=nimoos-message-bus.service`; `service/publisher.go` publishes events (indexing progress, etc.) to MessageBus; subscribes to `nimoos:media:created` / `nimoos:media:deleted` (index-on-write / clean-up-on-delete, `service/buscreated.go` / `busdelete.go`, each with its own separate WS connection; periodic full scans remain a fallback) |
| **nimoos-photos-ml-server** | Docker container, `127.0.0.1:3003`, called by `pkg/mlclient` via `/predict` + multipart/form-data; when ML is unavailable, the CLIP/face/OCR steps are skipped, and Embedder automatically backfills once it detects recovery; a hung state is handled with `docker restart` by the built-in watchdog (see Known issue 2) |
| **NimoOS-AI Agent / MCP server** | Can call `POST /search/smart` and `GET /albums` from localhost without a JWT (`mcpReadSkip`, see Authentication), serving as the backend for the `search_photos` / `list_albums` MCP tools |

---

## Known issues

1. **inotify quota amplification**: NimoOS-Photos's Watcher, Wiki (`NimoOS/`), and NimoOS-Search all run separate fsnotify instances watching directories like `/DATA/Gallery`. Each instance consumes its own inotify watches independently, so the pressure on `/proc/sys/fs/inotify/max_user_watches` stacks up. For very large directory trees, the quota may need to be raised manually (e.g. `echo 524288 > /proc/sys/fs/inotify/max_user_watches`). Each service currently keeps its own independent watcher (option A); a unified shared watch layer is a future TODO. On inotify **event queue overflow** (`fsnotify.ErrEventOverflow`), Watcher triggers a full-root recovery rescan (`service/watcher.go`); the rescan is single-flighted (`overflowRescanning`) with a 5-minute cooldown (`overflowRescanCooldown`) to prevent chained rescans during a write storm.

2. **ML service offline**: the ML container (nimoos-photos-ml-server) ships as an offline image bundle, requiring `docker load` via the install script the first time. While the container isn't running, CLIP embedding, face recognition, and OCR are all skipped (`ml.IsReady()` returns false); Embedder automatically backfills historical assets once it detects a `false→true` transition.

   **ML watchdog (defense in depth)**: the historical "port listening, worker is an empty shell" hang mode was specific to immich-ml's gunicorn-fronted architecture (a cold-load timeout could kill a worker mid-load while the master process kept listening). The in-house `mlserver` is a single uvicorn process where inference runs in a worker thread (`mlserver/server/main.py`), so `/ping` stays responsive even during a cold model load — that specific wedge mode is gone. The built-in watchdog `MLWatchdog` (`service/mlwatchdog.go`) is kept anyway as a second line of defense against container-level wedges (a runtime crash, a docker-level stall, anything that leaves the container "running" per docker but unresponsive): it probes every 30s and, only after 12 consecutive `/ping` failures (about 6 minutes — a threshold carried over unchanged from the immich-ml era, not tuned to any particular startup cost) and confirming via `docker inspect` that the container is running, issues a `docker restart`, with a 10-minute cooldown; if the container isn't running (ML package not installed / manually stopped by the user), it silently skips and resets the counter. Compose also has its own healthcheck (`start_period: 120s` — there's no gunicorn cold-compile tax to wait out anymore; a fresh OpenVINO EP compile on the very first run is instead covered by `install.sh`'s up-to-300s readiness poll), but that's only for observability via `docker ps` / AppManagement — it doesn't drive restarts.

3. **Video thumbnail used for CLIP embedding**: the CLIP embedding for a video is generated from the ffmpeg-extracted key frame (rather than the raw key frame), unified with the image path as `small.jpg`; this is intentional — it avoids a high-detail key frame ranking inappropriately high above photos in semantic search. The marker file `.clip_reembed_thumb_v1.done` prevents re-embedding after an index rebuild.

4. **TUS upload vs. fsnotify race**: after a TUS upload completes, `MarkAndReserve` claims a placeholder first, then rename happens, then `SubmitReserved` — preventing the Watcher's Create event from racing ahead into the anonymous batch slot (`batches[""]`), which would garble front-end progress reporting.

5. **Orphan reconciliation for caption backflow**: `Puller` (`service/captionpull.go`) periodically pulls the full caption diff-upsert from NimoOS-Parser into the local `asset_caption` table (used for Smart Moments topic matching). If a pulled asset ID no longer exists locally in `assets` (a true orphan, usually because a delete notification was lost and Parser-side data wasn't cleaned up accordingly), besides skipping the local write, it also best-effort deletes the corresponding vector back on the Parser side — a fallback reconciliation for cases where the "clean-up-on-delete" chain drops an event. `asset_caption` itself is cleaned up automatically via the `asset_id` foreign key's `ON DELETE CASCADE` when `assets` rows are deleted, so no separate Prune logic is needed.

6. **Face-thumbnail crop/rotation happens at serve time, not in the ML pipeline**: the ML detector's face bbox is always in the source image's raw, pre-rotation pixel space — `mlserver`'s face pipeline deliberately disables EXIF-transpose auto-rotation (`cv2.IMREAD_IGNORE_ORIENTATION`) to match the original immich-ml baseline. `PersonService.FaceThumbnail` (`service/persons.go`) therefore crops the square face avatar in that same raw coordinate space and only then rotates the cropped square per `asset_exif.orientation` (skipped for video sources, whose extracted key frame carries no independent rotation tag). Crops are cached to disk under `face-thumbs/<face_id>.jpg` (see Data storage) and orphans (rows with no matching `face_detections`) are reclaimed by the daily Prune, but the cache is not content-versioned — **`POST /cache/prune` will not fix stale crops**, since those files still have a live `face_detections` row, they're just cropped/rotated by the old logic. **One-time OPS step after deploying this fix: manually clear the `face-thumbs/` directory under `DataPath` once** (e.g. `rm -rf <DataPath>/face-thumbs/*`); the endpoint regenerates each crop on demand from the raw source image on next request. Separately, if a person's cover asset is trashed/offline, `FaceThumbnail` falls back to that person's largest live face rather than 404ing; this fallback result is not persisted back to `cover_face_id` — durable repair is left to the periodic `ClearDanglingCovers` / re-clustering pass (see Core flows §3).

---

## Startup order and deployment

systemd dependency (see `build/sysroot/usr/lib/systemd/system/nimoos-photos.service`):

```
nimoos-message-bus.service ──▶ nimoos-photos.service
```

`Type=notify`; considered started only after `SdNotify(Ready)`.

**Rollout notes for the timeline/pagination/storage perf changes** (see "Timeline/scale indexes" and the `/favorites`, `/trash`, `/storage` route rows above):
- First boot after upgrading past this change runs the one-time `ANALYZE assets` and creates the two partial indexes — on a ~500k-row library this is expected to take on the order of seconds, not minutes. Every boot after that only runs `PRAGMA optimize`.
- The `/favorites` and `/trash` list endpoints' absent-limit semantics changed to default to 500 rows instead of returning everything. **The frontend PR (`NimoOS-UI`, branch `perf/photos-timeline-scale`) must ship in the same rollout batch as this backend change** — an old frontend talking to the new backend will see at most 500 rows in those two views until it's updated to page/load-more past that cap.
- `/timeline` is kept for backward compatibility but is deprecated for scale; new frontend code should move to `/timeline/buckets` + `/timeline/bucket`.

**Rollout note for the `ClusterEpsilon` default change** (see Core flows §3 "Face clustering parameters"): the new `0.48` default only takes effect the next time faces are (re-)clustered — it does not retroactively touch an already-clustered library. After deploying this change, trigger `POST /v1/photos/persons/recluster` once (or just wait for the daily 03:xx scheduled pass, see `StartScheduler`) so unnamed clusters rebuild under the new epsilon and the old garbage mega-cluster dissolves. Named/favorited/related/hidden persons are anchored (`personAnchoredCond`) and keep their identity/cover through this pass regardless of epsilon — only unnamed auto-clusters get rebuilt.

```bash
# Build
CGO_ENABLED=1 go build -o nimoos-photos .

# Deploy (replace binary and restart)
bash scripts/deploy.sh photos
```

The ML backend is deployed independently (`deploy/ml/`) as a single offline distribution bundle covering every supported device — CPU and Intel iGPU via OpenVINO EP — from one image; there is no more per-vendor bundle split. The old **openvino**/**rocm**/**cpu** flavors, each pulling the official `immich-machine-learning` image (ghcr.io/immich-app), are gone along with immich-ml itself, replaced by the in-house `mlserver/` FastAPI server built from source:

- **Packaging** (`script/package-photos-ml.sh [version] [output dir]`, run once on a machine with internet access to huggingface.co + modelscope.cn): `docker build`s `mlserver/` into `localhost/nimoos-photos-ml:bundled` and `docker save`s it to `immich-ml.tar` (filename kept for `install.sh` compatibility), then `mlserver/golden/download_models.py` fetches the CLIP/face/OCR weights straight from their upstream HF/rapidocr repos into the `ml-cache` layout `ModelRegistry` expects (model names are still regex-extracted from `common/constants.go`, so the bundle always matches the code's model selection — no more "warm a temp container with fake predict requests to trigger a download" step). Both are packed with `docker-compose.yml`, `install.sh`, and `overrides/` into a single `photos-ml-universal-v<version>.tar.gz` (+ `.sha256` sidecar).
- **Installation** (`install.sh`, idempotent, re-running it updates to the bundled image version): only accepts a `FLAVOR=universal` bundle, failing loudly on an old-style per-flavor (cpu/openvino/rocm) bundle rather than silently booting the wrong image with the wrong environment; `detect_gpu_vendor` (via `/sys/class/drm/card*/device/vendor`) no longer gates the install on a vendor match — its only remaining job is deciding whether to lay down `overrides/openvino.yml` as `docker-compose.override.yml` for Intel iGPU device passthrough (`/dev/dri`). AMD and no-iGPU machines get no override and run CPU-only (this in-house server has no ROCm execution provider). Creates `ml-cache/ov-cache` up front for the OpenVINO EP's compiled-kernel cache; extracts the bundled model tar into `ml-cache` (skipped if already present, `FORCE_MODELS=1` forces an overwrite); 5-step flow: `docker load` → deploy compose + `.env` + override → prepare model cache → `docker compose up -d` → poll `/ping` (up to 300s — only the very first run against a fresh `ov-cache` pays the OpenVINO EP compile cost).
- **Runtime environment variables** (`docker-compose.yml`): `MLSERVER_CACHE=/cache`, `MLSERVER_TTL=300` seconds idle auto-unloads models to free memory (SigLIP2 SO400M + PP-OCRv5_server are fairly memory-hungry when resident; unloading drops native memory via `malloc_trim` in the registry's TTL sweeper, `mlserver/server/registry.py`), `MLSERVER_DEVICE=auto` (OpenVINO GPU EP when an Intel iGPU is present, CPU fallback otherwise; `mlserver/server/providers.py`'s provider list is resolved once at startup, when `mlserver/server/main.py` constructs the `ModelRegistry` — the GPU→CPU fallback itself happens at each model's first (lazy) load, i.e. ONNX Runtime session creation, not per request; always FP32 for numerical parity with the original immich-ml golden baseline). `mem_limit`/`memswap_limit` are capped at `8g` (the ML process has repeatedly dragged the whole box into a global OOM in the past). The healthcheck (`start_period: 120s`) is shorter than before since there's no gunicorn cold-compile tax to wait out; the healthcheck itself remains observability-only — actual self-healing is the nimoos-photos built-in watchdog (see Known issue 2).
- **Golden parity validation** (`mlserver/golden/`): `collect_baseline.py` captured ground-truth `/predict` responses from the original immich-ml container; `compare_clip.py` / `compare_faces.py` / `compare_ocr.py` replay the same inputs against mlserver and diff the outputs — the reference check to rerun before shipping any future model or dependency bump.

**Rollout note for the face quality signals (frontality/sharpness, see Core flows §8)**: this is a `mlserver`-source-only change (`mlserver/server/facemodel.py`) — it requires rebuilding the `localhost/nimoos-photos-ml:bundled` image (or repackaging via `script/package-photos-ml.sh` + `install.sh` for an offline install) and recreating the container; the Go side (`pkg/mlclient`, `pkg/sqlite`, `service/persons.go`) is already forward-compatible and will simply keep writing/treating the new columns as `NULL` until the rebuilt image is running. **Hard rule: whenever the container is rebuilt/recreated for this or any other `mlserver` code change, it must come back up with the GPU override applied** — recreating with only `docker-compose.yml` (no `-f docker-compose.yml -f docker-compose.override.yml`, or running outside the app's compose directory where default file resolution would pick both up) silently drops the Intel iGPU device passthrough and falls back to CPU-only inference (symptom: `[OpenVINO] Device GPU is not available` in the container logs, OCR throughput dropping ~4x). Verify with `docker inspect --format '{{range .HostConfig.Devices}}{{.PathOnHost}}{{end}}' <container>` — it must print `/dev/dri`. Existing (legacy) `face_detections` rows are not backfilled/rescanned for `frontality`; they keep it `NULL` indefinitely (expected), while photos indexed after the rebuild pick up both signals going forward.

**Deploy note for the sharpness backfill (see Core flows §8 "Legacy data")**: `sharpness`, unlike `frontality`, *is* backfilled for legacy rows, but not synchronously at deploy time — it's a Go-side, ML-independent one-shot pass (`BackfillSharpness`) that starts itself automatically `SharpnessBackfillStartupDelay` (3 minutes) after the first process start following this deploy, guarded by the `.face_sharpness_backfill_v1.done` marker so it never repeats. No manual trigger or ops step is needed; it doesn't require the `mlserver` container rebuild above at all (it's pure Go, reusing the same source-image resolution as detection). If it needs to be re-run (e.g. after a bug fix to the scoring itself), delete the marker file under `DataPath` and restart.

```bash
# Packaging (one-time, requires internet access)
script/package-photos-ml.sh 1.0.0

# Target machine: extract the distribution bundle then install/update (idempotent)
tar -xzf photos-ml-universal-v1.0.0.tar.gz -C /tmp/photos-ml
/tmp/photos-ml/install.sh
```
