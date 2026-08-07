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
