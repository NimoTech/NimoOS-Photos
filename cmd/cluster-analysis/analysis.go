package main

import (
	"fmt"
	"sort"
)

// ---------- Q1: chaining anatomy ----------

// buildGraph returns an adjacency list over local indices 0..len(indices)-1
// for the induced subgraph (indices restricted to eps-edges from the full
// matrix).
func buildGraph(indices []int, mat []float32, N int, eps float32) [][]int {
	m := len(indices)
	adj := make([][]int, m)
	for i := 0; i < m; i++ {
		gi := indices[i]
		base := gi * N
		for j := 0; j < m; j++ {
			if i == j {
				continue
			}
			if mat[base+indices[j]] <= eps {
				adj[i] = append(adj[i], j)
			}
		}
	}
	return adj
}

// articulationPoints finds cut vertices of the (assumed connected) graph adj
// via the standard DFS low-link algorithm, and for each one, the size of the
// second-largest resulting component if that vertex were removed.
func articulationPoints(adj [][]int) (apLocalIdx []int, splitSize []int) {
	n := len(adj)
	disc := make([]int, n)
	low := make([]int, n)
	visited := make([]bool, n)
	parent := make([]int, n)
	isAP := make([]bool, n)
	for i := range parent {
		parent[i] = -1
	}
	timer := 0

	var dfs func(u int)
	dfs = func(u int) {
		visited[u] = true
		timer++
		disc[u] = timer
		low[u] = timer
		children := 0
		for _, v := range adj[u] {
			if !visited[v] {
				children++
				parent[v] = u
				dfs(v)
				if low[v] < low[u] {
					low[u] = low[v]
				}
				if parent[u] == -1 && children > 1 {
					isAP[u] = true
				}
				if parent[u] != -1 && low[v] >= disc[u] {
					isAP[u] = true
				}
			} else if v != parent[u] {
				if disc[v] < low[u] {
					low[u] = disc[v]
				}
			}
		}
	}
	for i := 0; i < n; i++ {
		if !visited[i] {
			dfs(i)
		}
	}

	for u := 0; u < n; u++ {
		if !isAP[u] {
			continue
		}
		apLocalIdx = append(apLocalIdx, u)
		// BFS the graph with u removed; find component sizes.
		vis2 := make([]bool, n)
		vis2[u] = true
		var sizes []int
		for s := 0; s < n; s++ {
			if vis2[s] {
				continue
			}
			size := 0
			queue := []int{s}
			vis2[s] = true
			for len(queue) > 0 {
				cur := queue[0]
				queue = queue[1:]
				size++
				for _, v := range adj[cur] {
					if !vis2[v] {
						vis2[v] = true
						queue = append(queue, v)
					}
				}
			}
			sizes = append(sizes, size)
		}
		sort.Sort(sort.Reverse(sort.IntSlice(sizes)))
		if len(sizes) >= 2 {
			splitSize = append(splitSize, sizes[1])
		} else {
			splitSize = append(splitSize, 0)
		}
	}
	return
}

func nearestNeighborDists(indices []int, mat []float32, N int) []float64 {
	m := len(indices)
	out := make([]float64, m)
	for i := 0; i < m; i++ {
		gi := indices[i]
		base := gi * N
		best := float32(2.0)
		for j := 0; j < m; j++ {
			if i == j {
				continue
			}
			d := mat[base+indices[j]]
			if d < best {
				best = d
			}
		}
		out[i] = float64(best)
	}
	return out
}

func intraPairwiseDists(indices []int, mat []float32, N int) []float64 {
	m := len(indices)
	var out []float64
	for i := 0; i < m; i++ {
		gi := indices[i]
		base := gi * N
		for j := i + 1; j < m; j++ {
			out = append(out, float64(mat[base+indices[j]]))
		}
	}
	return out
}

func analyzeChaining(faces []Face, idOf map[string]int, garbageFaceIDs []string, named []namedPerson, mat []float32, N int) {
	garbageIdx := make([]int, 0, len(garbageFaceIDs))
	for _, fid := range garbageFaceIDs {
		if gi, ok := idOf[fid]; ok {
			garbageIdx = append(garbageIdx, gi)
		}
	}
	fmt.Printf("Garbage cluster: %d faces resolved to matrix indices\n", len(garbageIdx))

	// NN distance distribution within the garbage cluster.
	nn := nearestNeighborDists(garbageIdx, mat, N)
	mn, p10, med, p90, mx, mean := distStats(nn)
	fmt.Printf("Garbage cluster intra-NN-dist: min=%.4f p10=%.4f median=%.4f p90=%.4f max=%.4f mean=%.4f\n",
		mn, p10, med, p90, mx, mean)

	// Intra-cluster pairwise distance distribution, garbage vs named.
	gp := intraPairwiseDists(garbageIdx, mat, N)
	gmn, gp10, gmed, gp90, gmx, gmean := distStats(gp)
	fmt.Printf("Garbage cluster intra-PAIRWISE-dist (n=%d pairs): min=%.4f p10=%.4f median=%.4f p90=%.4f max=%.4f mean=%.4f\n",
		len(gp), gmn, gp10, gmed, gp90, gmx, gmean)

	fmt.Println("\nNamed-person intra-cluster pairwise distance (ground truth clusters):")
	for _, np := range named {
		var idxs []int
		for _, fid := range np.faceIDs {
			if gi, ok := idOf[fid]; ok {
				idxs = append(idxs, gi)
			}
		}
		if len(idxs) < 2 {
			fmt.Printf("  %-10s n=%d (too few faces for pairwise stats)\n", np.name, len(idxs))
			continue
		}
		pd := intraPairwiseDists(idxs, mat, N)
		mn, p10, med, p90, mx, mean := distStats(pd)
		fmt.Printf("  %-10s n=%d pairs=%d min=%.4f p10=%.4f median=%.4f p90=%.4f max=%.4f mean=%.4f\n",
			np.name, len(idxs), len(pd), mn, p10, med, p90, mx, mean)
	}

	// Bridge / articulation point analysis at eps=0.60 (production epsilon),
	// restricted to the garbage cluster's own member graph.
	fmt.Println("\nBridge-face (articulation point) analysis at eps=0.60, restricted to garbage-cluster members:")
	adj := buildGraph(garbageIdx, mat, N, 0.60)
	var degSum int
	for _, a := range adj {
		degSum += len(a)
	}
	fmt.Printf("  graph: %d nodes, avg degree=%.2f\n", len(garbageIdx), float64(degSum)/float64(len(garbageIdx)))
	apLocal, splitSizes := articulationPoints(adj)
	fmt.Printf("  articulation points (cut vertices): %d / %d\n", len(apLocal), len(garbageIdx))
	// Bucket by how "damaging" the cut is (second-largest resulting component size).
	var trivial, small, large int
	type apInfo struct {
		faceID string
		split  int
	}
	var infos []apInfo
	for k, li := range apLocal {
		s := splitSizes[k]
		infos = append(infos, apInfo{faceID: faces[garbageIdx[li]].ID, split: s})
		switch {
		case s <= 1:
			trivial++
		case s < 10:
			small++
		default:
			large++
		}
	}
	fmt.Printf("  split severity: trivial(<=1 face cut off)=%d  small(2-9)=%d  large(>=10)=%d\n", trivial, small, large)
	sort.Slice(infos, func(i, j int) bool { return infos[i].split > infos[j].split })
	fmt.Println("  top 10 most damaging bridge faces (face_id, size of 2nd-largest resulting component):")
	for i := 0; i < len(infos) && i < 10; i++ {
		fmt.Printf("    %s  split=%d\n", infos[i].faceID, infos[i].split)
	}
}

// ---------- Q2: epsilon sweep ----------

type SweepResult struct {
	Eps          float64
	MinPts       int
	NumClusters  int
	MaxSize      int
	FragCount    int
	FragPct      float64
	MeanRecall   float64
	MeanPurity   float64
	GarbageRetIn int     // # of original garbage-cluster faces landing in the new max-size cluster
	GarbageRetPc float64 // as fraction of original garbage cluster size
	PerPerson    []PersonCoherence
}

type PersonCoherence struct {
	Name    string
	Total   int
	InMaj   int
	Recall  float64
	Purity  float64
	ClusLbl int
	ClusSz  int
}

// evalCombo runs dbscanSubset(indices, eps, minPts) and computes the full
// SweepResult metric bundle (cluster sizes, fragmentation, per-named-person
// recall/purity, garbage-cluster retention). Shared by epsilonSweep (full
// face set) and gateSimulation (bbox-gated subsets).
func evalCombo(indices []int, eps float64, minPts int, idOf map[string]int, named []namedPerson, garbageFaceIDs []string, mat []float32, N int) SweepResult {
	garbageSet := map[int]bool{}
	for _, fid := range garbageFaceIDs {
		if gi, ok := idOf[fid]; ok {
			garbageSet[gi] = true
		}
	}
	localPos := map[int]int{}
	for li, gi := range indices {
		localPos[gi] = li
	}

	labels := dbscanSubset(indices, mat, N, eps, minPts)
	sizes := clusterSizes(labels)
	maxLbl, maxSz := maxClusterSize(sizes)

	fragCount := 0
	for _, s := range sizes {
		if s == 1 {
			fragCount++
		}
	}

	var recallSum, puritySum float64
	var perPerson []PersonCoherence
	for _, np := range named {
		labelCount := map[int]int{}
		total := 0
		for _, fid := range np.faceIDs {
			gi, ok := idOf[fid]
			if !ok {
				continue
			}
			li, ok2 := localPos[gi]
			if !ok2 {
				continue // face removed by gate
			}
			total++
			labelCount[labels[li]]++
		}
		if total == 0 {
			continue
		}
		bestLbl, bestCnt := -1, 0
		for l, c := range labelCount {
			if c > bestCnt {
				bestCnt = c
				bestLbl = l
			}
		}
		recall := float64(bestCnt) / float64(total)
		purity := float64(bestCnt) / float64(sizes[bestLbl])
		recallSum += recall
		puritySum += purity
		perPerson = append(perPerson, PersonCoherence{
			Name: np.name, Total: total, InMaj: bestCnt, Recall: recall, Purity: purity,
			ClusLbl: bestLbl, ClusSz: sizes[bestLbl],
		})
	}
	n := len(perPerson)
	meanRecall, meanPurity := 0.0, 0.0
	if n > 0 {
		meanRecall = recallSum / float64(n)
		meanPurity = puritySum / float64(n)
	}

	garbageRetIn := 0
	garbageOrigCount := 0
	for gi := range garbageSet {
		if li, ok := localPos[gi]; ok {
			garbageOrigCount++
			if labels[li] == maxLbl {
				garbageRetIn++
			}
		}
	}
	garbageRetPc := 0.0
	if garbageOrigCount > 0 {
		garbageRetPc = float64(garbageRetIn) / float64(garbageOrigCount)
	}

	return SweepResult{
		Eps: eps, MinPts: minPts, NumClusters: len(sizes), MaxSize: maxSz,
		FragCount: fragCount, FragPct: float64(fragCount) / float64(len(indices)),
		MeanRecall: meanRecall, MeanPurity: meanPurity,
		GarbageRetIn: garbageRetIn, GarbageRetPc: garbageRetPc,
		PerPerson: perPerson,
	}
}

func epsilonSweep(idOf map[string]int, named []namedPerson, garbageFaceIDs []string, mat []float32, N int, indices []int) []SweepResult {
	var results []SweepResult
	for e100 := 40; e100 <= 60; e100 += 2 {
		eps := float64(e100) / 100.0
		for minPts := 1; minPts <= 3; minPts++ {
			results = append(results, evalCombo(indices, eps, minPts, idOf, named, garbageFaceIDs, mat, N))
		}
	}
	return results
}

func printPerPersonDetail(results []SweepResult, eps float64, minPts int) {
	for _, r := range results {
		if r.Eps == eps && r.MinPts == minPts {
			fmt.Printf("  per-person detail @ eps=%.2f minPts=%d:\n", eps, minPts)
			for _, pc := range r.PerPerson {
				fmt.Printf("    %-10s total=%3d inMajorityCluster=%3d recall=%.3f clusterSize=%5d purity=%.3f\n",
					pc.Name, pc.Total, pc.InMaj, pc.Recall, pc.ClusSz, pc.Purity)
			}
			return
		}
	}
}

func printSweepTable(results []SweepResult) {
	fmt.Printf("%6s %6s %8s %8s %8s %8s %9s %9s %10s\n",
		"eps", "minPts", "#clust", "maxSize", "frag#", "frag%", "meanRecl", "meanPur", "garbRet%")
	for _, r := range results {
		fmt.Printf("%6.2f %6d %8d %8d %8d %7.1f%% %9.3f %9.3f %9.1f%%\n",
			r.Eps, r.MinPts, r.NumClusters, r.MaxSize, r.FragCount, r.FragPct*100,
			r.MeanRecall, r.MeanPurity, r.GarbageRetPc*100)
	}
}

// ---------- Q3: size-gate simulation ----------

func gateSimulation(faces []Face, idOf map[string]int, named []namedPerson, garbageFaceIDs []string, mat []float32, N int, fullSweep []SweepResult) {
	thresholds := []float64{0.02, 0.04, 0.06}
	gateIndices := map[float64][]int{}
	for _, th := range thresholds {
		var kept []int
		removed := 0
		for i, f := range faces {
			if !f.HasExif {
				kept = append(kept, i) // no exif -> always keep
				continue
			}
			ratio := f.MinSide / f.ImgMinD
			if ratio < th {
				removed++
				continue
			}
			kept = append(kept, i)
		}
		gateIndices[th] = kept
		fmt.Printf("Gate %.0f%%: removes %d / %d faces (%.1f%%), keeps %d\n",
			th*100, removed, N, float64(removed)/float64(N)*100, len(kept))
	}

	// Pick the best 2-3 (eps,minPts) combos from the full sweep to re-run
	// under each gate. Selection heuristic: prefer combos that noticeably
	// shrink the max cluster (dissolve the mega-cluster) while keeping mean
	// named-person recall reasonably high and not exploding fragmentation.
	type cand struct {
		eps    float64
		minPts int
	}
	candidates := []cand{
		{0.44, 1},
		{0.46, 1},
		{0.48, 1},
	}
	fmt.Println("\nCandidate (eps,minPts) combos re-run under each gate (all minPts=1, since Q2 showed minPts>=2 gives no additional dissolution benefit while breaking sparse 2-face named identities):")
	for _, th := range thresholds {
		idxs := gateIndices[th]
		fmt.Printf("\n-- gate=%.0f%% (N=%d) --\n", th*100, len(idxs))
		var subResults []SweepResult
		for _, c := range candidates {
			subResults = append(subResults, evalCombo(idxs, c.eps, c.minPts, idOf, named, garbageFaceIDs, mat, N))
		}
		printSweepTable(subResults)
		fmt.Printf("  per-person detail @ eps=%.2f minPts=%d (gate=%.0f%%):\n", candidates[0].eps, candidates[0].minPts, th*100)
		for _, pc := range subResults[0].PerPerson {
			fmt.Printf("    %-10s total=%3d inMajorityCluster=%3d recall=%.3f clusterSize=%5d purity=%.3f\n",
				pc.Name, pc.Total, pc.InMaj, pc.Recall, pc.ClusSz, pc.Purity)
		}
	}
	_ = fullSweep
}
