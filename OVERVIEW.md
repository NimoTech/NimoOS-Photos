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
| GET | `/persons` | List of persons (face clusters) |
| GET | `/persons/merge-suggestions` | Merge suggestions |
| POST | `/persons/merge-suggestions/reject` | Reject a merge suggestion |
| POST | `/persons/merge` | Merge persons (400 if `from_id==into_id`; 404 if either person doesn't exist) |
| POST | `/persons/recluster` | Re-cluster |
| GET/PUT/DELETE | `/persons/:id` | Person detail/update/delete |
| POST | `/persons/:id/restore` | Restore a deleted person |
| GET | `/persons/:id/assets` | Person's photos (404 if the person is hidden or missing, via the `PersonVisible` guard) |
| GET | `/persons/:id/relations` | Related persons (404 if the person is hidden or missing, via `PersonVisible`) |
| GET | `/persons/:id/places` | Places this person appears in (404 if the person is hidden or missing, via `PersonVisible`) |
| GET | `/persons/:id/face-thumbnail` | Person face thumbnail (JWT-exempt; crops in raw pixel coordinates and rotates per `asset_exif.orientation` at serve time; falls back to a live face if the cover asset is trashed/offline) |
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

### 4. Embedder backfill

`Embedder.Run` checks ML readiness every 30 seconds:
- On a **false → true** transition, triggers asynchronously:
  1. `Backfill`: backfill embeddings for all assets with `status='indexed'` but missing a CLIP vector
  2. `reembedThumbnailsOnce`: one-time recomputation of all existing embeddings from thumbnails (marker file `.clip_reembed_thumb_v1.done` prevents repeating this)
  3. `BackfillOCR`: backfill for all assets missing OCR text

### 5. Reverse geocoding

`GeoService` reads GPS coordinates from `asset_exif` and reverse-geocodes them offline using an embedded gazetteer (`pkg/geo/data/*.tsv.gz`: 15,000+ cities, countries, POIs), writing to `asset_geo`. When the gazetteer version (`geoGazVersion`) changes, `asset_geo` is automatically cleared and rerun.

### 6. Automatic rebuild on ML model generation change

`common.MLModelGen` (currently `"3"` = SigLIP2 SO400M + antelopev2 + PP-OCRv5_server; gen 2 was the nllb-clip-large era) identifies the model combination (CLIP/face/OCR selection + dimensions) bound to the current binary; it's written to `photos_meta.ml_model_gen` after a successful rebuild. At startup, if this key is missing (old DB) or doesn't match the current `MLModelGen`, `Rebuilder.MaybeAutoRebuild` (`service/rebuild.go`) polls until the ML backend is ready (new model cache in place) and then automatically triggers a full rebuild: clears `clip_embeddings`/`asset_clip_idx`, reruns CLIP/face/OCR on all `status='indexed'` assets, re-clusters faces and cleans up faceless empty `persons`, and finally writes back the new generation. An empty library (no assets) skips the worker pool, goes straight to re-clustering and writing the generation, completing in seconds. The generation is only written after the rebuild finishes successfully (`finalize()`); a mid-rebuild failure or power loss will retry on the next startup.

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
- Person cover hybrid score (`service/persons.go` `hybridCoverScore`: whole-image aesthetic score × that face's bbox area ratio × the face detection's own confidence (`face_detections.score`, clamped to `[0,1]`; `NULL` on rows written before the column existed is treated as neutral, i.e. a factor of `1`); the aesthetic/area factors must be comparable — an unscored asset/missing EXIF width-height/degenerate bbox is marked incomparable; unaffected when `cover_locked=1`, which still skips automatic recomputation)
- Person hero fallback (`service/persons.go`: list/detail hero query when there's no locked cover)

A manually specified cover (`cover_asset_id`/`cover_face_id`/`cover_locked=1`) always takes priority over the aesthetic score; when the whole-library score is NULL (e.g. during a rescore right after switching heads), all five sites above fall back to their own previous ranking (time/position, etc.).

Config toggle `AestheticEnabled` (`photos.conf`, default `true`), **not hot-reloaded**: turning it off only stops new scoring (inline is skipped, `BackfillAesthetic` isn't triggered); existing scores aren't cleared. Turning it back on requires restarting the service to load the head.

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
| `face_detections` | Face detection results (bbox, 512-dim embedding, excluded flag, `score` REAL = detector confidence, `NULL` on legacy rows predating the column = neutral factor in cover selection) |
| `persons` | Face clustering results (name, cover, centroid, confidence; `hidden` flag drives the `PersonVisible` 404 guard; `cover_locked`/`hero_asset_id` anchor a pinned cover/hero through re-clustering) |
| `face_person` | Face → person mapping |
| `asset_ocr` | OCR text (coverage, line_count density-candidate gate; boxes_ver=0 means per-line coordinates aren't stored; doc_sem/doc_geo/is_doc are the semantic margin/geometric regularity/final verdict of the mixed criterion (NULL=not yet computed, query falls back to pure density), doc_ver=0 means pending computation; the single write path is computeDocVerdict()) |
| `asset_ocr_lines` | OCR per-line text + normalized four-corner coordinates (JSON, 8 floats, [0,1]); line_no matches the concatenation order in asset_ocr.text; single write path is ocrAsset(); cascade-deletes with assets via foreign key; used for GET /assets/:id/ocr search-hit highlighting and doc geometric-regularity computation |
| `clip_text_cache` | CLIP text-prompt vector cache (key=prompt, gen=MLModelGen); used for the doc-classification zero-shot criterion; auto-invalidated and re-embedded on a model generation change |
| `asset_geo` | Reverse-geocoded location info (city, country, geonameid) |
| `albums` + `album_assets` | Manual albums (supports ordering) |
| `asset_favorites` | Per-user favorites |
| `asset_views` | Per-user view counts |
| `smart_views` + `smart_view_matches` | Semantic auto-albums and their match results |
| `merge_rejections` | Rejected face-merge suggestion pairs |
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

```bash
# Packaging (one-time, requires internet access)
script/package-photos-ml.sh 1.0.0

# Target machine: extract the distribution bundle then install/update (idempotent)
tar -xzf photos-ml-universal-v1.0.0.tar.gz -C /tmp/photos-ml
/tmp/photos-ml/install.sh
```
