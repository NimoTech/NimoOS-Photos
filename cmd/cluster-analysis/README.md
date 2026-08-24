# cluster-analysis

Offline, read-only parameter-tuning tool for the face-clustering DBSCAN step
(`service.DBSCAN` / `service.DBSCANWithProgress` in `NimoOS-Photos/service`).
It was built to answer one question during the clustering-quality project
(P0/P1 fixes, `fix/people-p0p1`): **why did one unnamed person end up with a
2612-face "mega cluster", and what epsilon/minPoints/size-gate configuration
dissolves it without breaking real, sparsely-sampled identities?**

It reuses the exact production algorithm (`service.DBSCAN`,
`service.DBSCANWithProgress`, `service.ComputeCentroid`,
`service.ClusterConfidence`) for calibration and timing, plus a locally
reimplemented matrix-backed DBSCAN variant (`dbscanSubset` in
`analysis.go`) that precomputes the full pairwise cosine-distance matrix
once and reads from it during a parameter sweep — this is what makes a
33-combo epsilon x minPoints sweep (plus size-gate re-runs) tractable in a
few seconds instead of re-running an O(n^2) distance computation per combo.

**This tool is not wired into any build or deploy path.** It is not
imported by any other package and is not part of the service binary; it
exists purely for one-off / repeatable offline analysis against a
*read-only copy* of a production database.

## Safety

- Opens the DB with `file:<path>?mode=ro` — never writes.
- Never point `-db` at a live production database path (e.g. anything under
  `/DATA/.system_data`). Always run against a throwaway copy.

## Usage

```
CGO_ENABLED=1 go run ./cmd/cluster-analysis \
  -db /path/to/readonly/photos.db \
  -eps 0.48 \
  -minpts 1 \
  -minconf 0.5
```

Flags:

| Flag | Default | Meaning |
|---|---|---|
| `-db` | (required) | Path to a **read-only copy** of the Photos sqlite DB |
| `-eps` | `0.48` | DBSCAN cosine-distance epsilon to validate (production default per C1's `ClusterEpsilon` config) |
| `-minpts` | `1` | DBSCAN minPoints to validate |
| `-minconf` | `0.5` | `MinPersonConfidence` gate to compare the residual cluster's `ClusterConfidence` against (production default, B7) |
| `-mode` | `""` | `""` runs the legacy eps x minPoints DBSCAN calibration study below; `twopass` runs the two-pass (Apple engine) T_tight x T_merge grid scan described in "Two-pass grid calibration" below; `knn` runs the KNN exemplar-assignment T_auto x T_suggest calibration described in "KNN threshold calibration" below; `merge` runs the T-merge cluster-merge cut-point calibration described in "T-merge cut-point calibration" below |
| `-knnk` | `5` | `AssignKNNK`: number of nearest exemplars used per KNN median-distance computation, only read by `-mode knn` |

The historical production ground-truth calibration point (eps=0.60,
minPoints=1 — the legacy `dbscanEpsilon`/`dbscanMinPoints` constants in
`service/persons.go`) is fixed internally as `calibEpsilon`/`calibMinPoints`
and always run first, regardless of `-eps`/`-minpts`.

## What it does, in order

1. **Load faces**: all `excluded=0` face detections joined to a live asset
   (matches production's `loadFacesWithProgress` query), plus each face's
   bbox-min-side / image-min-dimension ratio (`asset_exif`) as a size-gate
   proxy — `face_detections.score` does not exist as a column and cannot be
   simulated.
2. **Load ground truth**: the known garbage-cluster person's member faces,
   and every named person's member faces.
3. **Build the full N x N cosine-distance matrix** (parallelized across all
   CPUs).
4. **Calibration 1** — run the matrix-backed `dbscanSubset` at
   (`calibEpsilon`, `calibMinPoints`) and check it reproduces a mega cluster
   comparable to the known production ground truth for this DB copy.
5. **Calibration 2** — run the *real* `service.DBSCAN` (not the local
   reimplementation) at both `(calibEpsilon, calibMinPoints)` and
   `(-eps, -minpts)`, and assert its cluster partition is **exactly** (not
   approximately) identical to `dbscanSubset`'s, up to cluster-ID
   relabeling. This is what makes every sweep/validation number below
   trustworthy as a faithful readout of production behavior rather than an
   artifact of the faster local reimplementation. Aborts (`log.Fatal`) on
   any mismatch.
6. **D2 timing** — times `service.DBSCAN` (serial `regionQuery`) against
   `service.DBSCANWithProgress` (D2's parallel neighbor-list precompute) on
   the real production embeddings at `-eps`/`-minpts`, and asserts their
   output labels are identical (a timing-only divergence would itself be a
   D2 regression).
7. **Q1 — chaining anatomy**: nearest-neighbor / intra-cluster pairwise
   distance distributions for the garbage cluster vs named-person clusters,
   plus an articulation-point (cut-vertex) analysis of the garbage cluster's
   eps=0.60 adjacency graph to find "bridge" faces holding it together.
8. **Q2 — epsilon x minPoints sweep**: the full percolation-cliff table
   (below).
9. **Q3 — bbox size-gate simulation**: re-runs a few candidate combos under
   simulated size gates (2%/4%/6% of image min-dimension) to check whether
   gating small face crops helps dissolve the mega cluster.
10. **Final validation**: for `-eps`/`-minpts`, prints max cluster size,
    per-person purity, the residual largest cluster's `ClusterConfidence`
    vs the `-minconf` gate, and garbage-cluster retention.

## Two-pass grid calibration (`-mode twopass`)

`-mode twopass` calibrates the two-pass "apple" clustering engine
(`service.SegmentMoments` -> `service.GreedyMomentClusters` ->
`service.HACComplete`, wired behind `photos.ClusterEngine`) instead of the
legacy single-pass DBSCAN engine above. It reuses these three exported
service functions directly -- no algorithm is reimplemented here, unlike
the legacy mode's matrix-backed `dbscanSubset` fast-path.

For each gap in `{30, 60, 120}` minutes, it grid-scans:

- `T_tight` (`service.GreedyMomentClusters`'s within-moment union epsilon):
  `0.30..0.40` step `0.01` (11 values)
- `T_merge` (`service.HACComplete`'s complete-linkage stop distance):
  `0.45..0.60` step `0.01` (16 values)

176 combos per gap, 528 total. For a fixed gap, `SegmentMoments` runs once;
for a fixed (gap, T_tight), `GreedyMomentClusters` runs once and its pass-1
labels are reused across the whole `T_merge` sub-loop -- only
`HACComplete` (the expensive step) runs once per full combo.

Each combo's row reports: `#clusters`, `maxSize`, named-person `purity`
(mean over named persons of their majority-cluster purity -- reaches 1.0
only if every named person's majority cluster is 100% them), and `frag#`
(named-person fragment count: summed over named persons, `numClusters
containing that person's faces - 1`; zero iff nobody's faces are split
across clusters).

Each gap's table is sorted so the recommended combos surface at the top,
followed by a **selection criterion** block (also printed as the final
overall recommendation across all three gaps): **among purity==1.0 combos,
minimize the named-person fragment count, tie-break on smaller max
cluster size** -- the same standard the legacy eps=0.48 DBSCAN calibration
study used.

```
CGO_ENABLED=1 go run ./cmd/cluster-analysis -db /path/to/readonly/photos.db -mode twopass
```

## KNN threshold calibration (`-mode knn`)

`-mode knn` turns accumulated user feedback into a recommendation for the
KNN exemplar-assignment thresholds (`AssignAutoDist`/`AssignSuggestDist`/
`AssignKNNK` in `pkg/config`, consumed by `service/persons.go`'s
`assignAutoDist`/`assignSuggestDist`/`assignK` and enforced by
`service/matcher.go`'s `Match`), which currently run on engineering
defaults (0.45 / 0.60 / 5). Ground truth accumulates as `face_person` rows
with `confirmed=1` (user-confirmed: this face IS this person) and
`person_negatives` rows (user-rejected: this face is NOT this person).

It reads each anchored person's exemplar set directly from the persisted
`face_person.exemplar=1` flags -- it does **not** recompute exemplars via
`service.SelectExemplars` -- and, for every truth row, computes the exact
statistic the live matcher uses: the median distance of that person's
exemplars among the `k` (`-knnk`, default 5, `AssignKNNK`'s production
default) nearest of them to the face's embedding. This is not a
reimplementation: it builds a single-person `service.BuildExemplarIndex`
(the same "solo index" trick `service/faces.go`'s revalidation pass uses)
and calls the real `service.Match`, with `autoDist`/`suggestDist` forced to
`+Inf` so `Match` always reports its real median distance instead of
zeroing it on a "none" decision -- see `knnDistance`'s doc comment in
`knn.go` for the full reasoning. A truth row's own face is excluded from
its person's exemplar pool before matching (self-exclusion), so a confirmed
face that also happens to be one of its own person's exemplars can't report
a trivial, meaningless zero distance against itself.

### What it prints

1. **Distance distributions** for both truth sources: `count`/`min`/`q1`/
   `median`/`q3`/`max` plus a coarse 0.05-wide ASCII histogram, always
   printed even when the counts below are too small to trust a
   recommendation from -- the distribution shape is useful early signal on
   its own.
2. **Grid scan**: `T_auto` in `[0.35, 0.55]` step `0.01` x `T_suggest` in
   `[T_auto+0.05, 0.70]` step `0.01`. Each combo's row reports, over the
   truth set: `FA` (false-accept: negatives with `dist <= T_auto`, i.e.
   wrongly auto-joined), `TA` (true-accept: positives with `dist <=
   T_auto`), `grayP`/`grayN` (positives/negatives with `T_auto < dist <
   T_suggest`, correctly routed to human review either way), and `miss`
   (positives with `dist >= T_suggest`, never even reaching a suggestion).
3. **Selection criterion**, printed verbatim alongside the winning combo:
   among combos with **zero false-accepts** in the auto zone, **maximize
   true-accepts**, tie-break on **larger gray-zone coverage of negatives**
   (prefer routing more "not this person" truth to human review over
   missing it entirely).
4. **Insufficient-data guard**: when usable positives `< 100`, usable
   negatives `< 20`, or the number of distinct persons contributing at
   least one usable row `< 5`, a prominent warning block states the
   recommendation is **not yet trustworthy**, alongside the current counts
   vs. the bars. The tool still runs and prints the distributions/grid in
   full either way -- only the recommendation's trustworthiness is gated,
   not its visibility.

```
CGO_ENABLED=1 go run ./cmd/cluster-analysis -db /path/to/readonly/photos.db -mode knn -knnk 5
```

## T-merge cut-point calibration (`-mode merge`)

`-mode merge` turns accumulated, user-decided cluster-merge questions into a
proposed `ClusterMergeEps` cut point. Ground truth is the `merge_suggestions`
table's decided rows (`pkg/sqlite/db.go`): `status IN ('accepted','rejected')`,
each carrying the `dist` the two candidate persons' clusters were at when the
question was raised. `face_negative_pairs` is **not** consumed here -- it
stores no distance and stays an enforcement-side (cannot-link) table, not a
distance-truth source.

### What it prints

1. **Distance distributions** for accepted and rejected dists separately:
   `count`/`min`/`q1`/`median`/`q3`/`max` plus the same coarse 0.05-wide ASCII
   histogram `-mode knn` uses, always printed regardless of the bars below.
2. **Recommended cut point**: the largest accepted dist strictly below every
   rejected dist (zero-false-accept style, i.e. below `min(RejectedDists)`),
   or a "no valid cut point" message when no accepted dist qualifies (e.g.
   the smallest rejected dist is itself below every accepted dist), when
   either truth set is empty.
3. **Insufficient-data guard**: when total decided rows `< 30`, accepted
   `< 10`, rejected `< 5`, or distinct persons contributing at least one
   decided row `< 8`, a prominent warning block states the recommendation is
   **not yet trustworthy**, alongside the current counts vs. the bars. The
   distributions and recommendation still print in full either way -- only
   trustworthiness is gated.

```
CGO_ENABLED=1 go run ./cmd/cluster-analysis -db /path/to/readonly/photos.db -mode merge
```

## Relationship to in-service self-calibration

The `knn`/`merge`/`twopass` calibration cores this tool reports against — grid
scan, cut-point selection, insufficient-data bars — now live in the `service`
package (`service/calibrate_knn.go`, `service/calibrate_merge.go`,
`service/calibrate_twopass.go`), not in this `cmd/cluster-analysis` binary:
`nimoos-photos` runs the same recommendation logic unattended, in-process,
after every clustering pass (`service/calibrate_run.go`'s `maybeCalibrate`),
applying bounded adjustments to `AssignAutoDist`/`AssignSuggestDist`/
`ClusterTightEps`/`ClusterMergeEps`/`MomentGapMinutes` directly into
`calibration_state` under safety rails (step limits, hysteresis, cross-key
invariants — see `OVERVIEW.md`'s "Threshold self-calibration" section for the
full picture, including the `GET /v1/photos/persons/calibration` status
endpoint). This tool is now a **thin, read-only reporting front-end** over
those same shared cores: `-mode knn`/`-mode merge`/`-mode twopass` call the
exact same exported functions the live service uses (`LoadKNNTruth`/
`KNNGridScan`/`SelectKNNCombo`, `LoadMergeTruth`/`MergeCutPoint`,
`TwoPassGridScan`/`SelectTwoPassCombo`), so an offline report against a DB
copy and the live self-calibration runner can never silently diverge in
their recommendation logic — only in when/how the result gets applied. Use
this tool for an on-demand, human-reviewed second opinion (e.g. after a
library's camera/face-count/identity mix shifts enough to warrant sanity
checking the profile's `[Min,Max]` bands) rather than as the primary
calibration path, since the in-service runner now handles the day-to-day
adjustment automatically.

## Percolation-cliff sweep (eps x minPoints)

Full study over 4409 faces (excluded=0, joined to a live asset) in the
reference DB copy; ground truth = one 2612-face garbage cluster + 8 named
persons with face counts 35/8/4/2/2/2/1/1.

| eps | minPts | maxSize | frag% | meanRecall | meanPurity | garbRet% |
|---|---|---|---|---|---|---|
| 0.40 | 1 | 363 | 22.6% | 0.938 | 1.000 | 13.9% |
| 0.44 | 1 | 365 | 20.0% | 0.938 | 1.000 | 14.0% |
| 0.46 | 1 | 366 | 19.1% | 0.938 | 1.000 | 14.0% |
| 0.48 | 1 | 366 | 18.3% | 0.938 | 1.000 | 14.0% |
| **0.50** | **1** | **685** | **17.4%** | **1.000** | **1.000** | **26.2%** |
| 0.52 | 1 | 762 | 16.4% | 1.000 | 1.000 | 29.2% |
| 0.54 | 1 | 1045 | 15.8% | 1.000 | 1.000 | 40.0% |
| 0.56 | 1 | 1520 | 15.2% | 1.000 | 1.000 | 58.2% |
| 0.58 | 1 | 1828 | 14.8% | 1.000 | 1.000 | 70.0% |
| 0.60 (=production legacy) | 1 | 2612 | 14.2% | 1.000 | 1.000 | 100.0% |
| 0.40-0.60 | 2 | 363-2612 | 19.7-29.3% | 0.812 (flat) | 1.000 | same as minPts=1 |
| 0.40-0.60 | 3 | 363-2600 | 21.8-31.3% | 0.812 (flat) | 1.000 | ~same |

**Percolation cliff at eps~0.50**: at minPoints=1, `maxSize` is essentially
flat (363->366) for eps 0.40-0.48, then jumps discontinuously to 685 at
eps=0.50 and climbs monotonically back to the full 2612 by eps=0.60.
**Epsilon must stay <=0.48 to keep the mega cluster dissolved.**

**minPoints is the wrong knob**: raising minPoints from 1->2->3 gives
essentially zero extra dissolution at any eps (the garbage blob's average
degree of ~119 trivially satisfies minPoints=3 almost everywhere), while
roughly doubling fragmentation and permanently breaking three sparse 2-face
named identities (Ava/Liam/Noah, each an isolated mutual pair with no third
point ever within range) — `meanRecall` is capped at a flat 0.812 for every
eps once minPoints>=2. **minPoints=1 is required to keep low-sample
identities intact.**

**The bbox size-gate is not a safe primary lever**: at a 4% gate the
residual max cluster only shrinks 366->347 (-5%), while completely erasing
Oliver's only face and both of Ava's faces from the ground truth (2 of 8
named persons vanish) — the "recall=1.000" reading at that gate is inflated
by denominator collapse, not genuine coherence gain. Even the mild 2% gate
removes Oliver's sole face. **The bbox size-gate risks deleting legitimate
low-sample identities faster than it dissolves the actual chain**, and
should not be used as the primary dissolution lever.

**Recommended config**: `eps=0.48, minPts=1, no size gate` — best balance at
the safe edge of the eps<=0.48 sub-cliff band, dissolves the mega cluster by
86% (2612 -> 366) at the lowest fragmentation cost (+4.1pt over baseline)
among the dissolving options, with only Noah's 2-face pair (mutual distance
0.481) failing to merge.

## Latest validation (production DB copy, eps=0.48, minPts=1, minconf=0.5)

Run against the read-only production-copy DB (4409 faces, 8 named persons,
one known 2612-face garbage cluster), 2026-08-07, `CGO_ENABLED=1`, 16 CPUs:

- **Calibration 1** (eps=0.60/minPts=1 vs ground truth): matrix-backed
  `dbscanSubset` reproduces `#clusters=845 maxClusterSize=2612` exactly.
- **Calibration 2** (matrix-backed vs real `service.DBSCAN`): **PASS** —
  partition-identical at both eps=0.60/minPts=1 and eps=0.48/minPts=1.
  `service.DBSCAN` took 6.75s / 6.58s respectively over the full 4409-face
  set (confirms the fast local reimplementation is faithful, not a
  coincidence).
- **maxClusterSize = 366** (matches the sweep table's expectation of
  ~366, `#clusters=1077`).
- **Named-person purity = 1.0000** (n=8 persons; every named person's
  majority cluster is composed entirely of their own faces).
- **Residual largest cluster's `ClusterConfidence` = 0.8613, vs the
  `MinPersonConfidence=0.5` gate (B7) -> VISIBLE.** The B7 cohesion gate
  does **not** hide the residual 366-face cluster — 0.8613 is well above the
  0.5 floor, so if this cluster were exposed as an unnamed auto-person it
  would surface in the People list, not be filtered out. This is expected:
  `ClusterConfidence` measures average cosine similarity of members to
  their own cluster's centroid, which stays high even for a residual
  multi-identity fragment as long as its 366 members are still closer to
  *each other* than the eps=0.48 threshold allows — B7's floor catches
  low-cohesion clusters, not merely-imperfect ones. The mega-cluster
  problem is fixed by dissolving cluster *size* (this project's C1/eps
  fix), not by B7's independent confidence gate; the two are orthogonal
  safeguards.
- Garbage-cluster retention in the residual: 366/2612 faces (14.0%) — i.e.
  100% of the *dissolution* comes from the eps change; none of the original
  2612 garbage faces are filtered out by any gate, they are simply
  redistributed into ~1077 much smaller clusters (max 366).
- **D2 timing on real production embeddings** (eps=0.48, minPts=1, 4409
  faces, 512-dim, 16 CPUs): `service.DBSCAN` (serial) = 6.65s,
  `service.DBSCANWithProgress` (D2 parallel) = 0.61s, **speedup = 10.92x**,
  output labels identical. This is consistent with — slightly below,
  because real embeddings have a less uniform neighbor-count distribution
  than the synthetic random vectors used there — the D2 task report's own
  benchmark on the same-shaped synthetic data (4409x512, eps=0.48, 16
  cores): serial 6.69s, parallel 511ms, 13.09x
  (`task-D2-report.md`, "Performance" section).
