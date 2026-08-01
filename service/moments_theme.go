// The theme engine: takes the union of two paths' hits — the recipe's
// clip_prompts (CLIP semantic search) and caption_keywords (caption text
// keywords) — then intersects with the candidate pool (excluding trash/
// offline/documents/live photo companion videos), producing a single
// "rolling" theme moment draft once it reaches MinAssets (see ThemeMomentID's
// design: the same recipe always maps to the same moment id).
package service

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// AssetScore is the minimal result of one clipTextSearcher text-search hit:
// asset id + similarity score (convention [0,1], same scale as
// Asset.MatchScore).
type AssetScore struct {
	AssetID string
	Score   float64
}

// clipTextSearcher is the CLIP text-search capability the theme engine
// needs; the real implementation is SearchService.SearchAssetsByText
// (search.go). Tests inject a fake, so the theme engine doesn't directly
// depend on the ML layer.
type clipTextSearcher interface {
	SearchAssetsByText(ctx context.Context, prompt string, topK int) ([]AssetScore, error)
}

// BuildThemeMoments is the theme engine's entry point: for each of
// ClipPrompts, calls searcher for TopK, filtered by MinScore; CaptionKeywords
// does a word-boundary match against asset_caption (see
// matchCaptionKeywords, a hit gets the MinScore floor score); takes the
// union of both paths (an asset hit by both takes the max score). The union
// is then intersected with the candidate pool (the same candidate criteria
// as the trip engine, see loadThemeCandidatePool), and only produces a
// single MomentDraft once the member count reaches MinAssets; otherwise
// returns an empty slice (this recipe has no theme moment to show this
// round, not an error).
func BuildThemeMoments(ctx context.Context, db *sql.DB, searcher clipTextSearcher, recipe MomentRecipe) ([]MomentDraft, error) {
	params, err := ParseParams(recipe)
	if err != nil {
		return nil, err
	}

	// score is the accumulator table of "asset id → this round's final score
	// after taking the union of both paths".
	score := map[string]float64{}

	for _, prompt := range params.ClipPrompts {
		hits, err := searcher.SearchAssetsByText(ctx, prompt, params.TopK)
		if err != nil {
			return nil, fmt.Errorf("moments: theme clip search %q: %w", prompt, err)
		}
		for _, h := range hits {
			if h.Score < params.MinScore {
				continue
			}
			if cur, ok := score[h.AssetID]; !ok || h.Score > cur {
				score[h.AssetID] = h.Score
			}
		}
	}

	if len(params.CaptionKeywords) > 0 {
		hits, err := matchCaptionKeywords(ctx, db, params.CaptionKeywords)
		if err != nil {
			return nil, err
		}
		for _, id := range hits {
			// A keyword hit has no continuous similarity to use, so it gets the
			// MinScore floor score — sitting exactly at the filter line, it neither
			// gets filtered out by its own threshold nor overshadows a
			// high-confidence CLIP hit (when taking the max, a higher CLIP score
			// wins out).
			if cur, ok := score[id]; !ok || params.MinScore > cur {
				score[id] = params.MinScore
			}
		}
	}

	if len(score) == 0 {
		return nil, nil
	}

	pool, err := loadThemeCandidatePool(ctx, db)
	if err != nil {
		return nil, err
	}

	var assets []MomentAsset
	var from, to time.Time
	for id, s := range score {
		takenAt, ok := pool[id]
		if !ok {
			continue // Not in the candidate pool (trash/offline/document/live photo companion video/no taken_at).
		}
		assets = append(assets, MomentAsset{AssetID: id, Score: s})
		if from.IsZero() || takenAt.Before(from) {
			from = takenAt
		}
		if to.IsZero() || takenAt.After(to) {
			to = takenAt
		}
	}

	if len(assets) < params.MinAssets {
		return nil, nil
	}

	// Sorted by score descending, ties broken by id, guaranteeing a
	// deterministic, reproducible member order across recompute rounds (the
	// same stability requirement as the trip engine sorting by taken_at).
	sort.Slice(assets, func(i, j int) bool {
		if assets[i].Score != assets[j].Score {
			return assets[i].Score > assets[j].Score
		}
		return assets[i].AssetID < assets[j].AssetID
	})

	draft := MomentDraft{
		Moment: Moment{
			ID:         ThemeMomentID(recipe.Key),
			RecipeKey:  recipe.Key,
			Title:      recipe.Title,
			TimeFrom:   from,
			TimeTo:     to,
			AssetCount: len(assets),
		},
		Assets: assets,
	}
	return []MomentDraft{draft}, nil
}

// matchCaptionKeywords returns a deduplicated list of asset ids whose
// asset_caption text has a "word-boundary hit" for any of the keywords.
//
// A pitfall found in real-device testing: the early implementation directly
// reused docscore/ocrSearch's instr(lower(text), lower(?)) > 0 substring
// criterion; SQLite has no REGEXP, only substring matching, and the result
// was that "cat"⊂vacation/location, "pet"⊂carpet, "ice"⊂nice/service were
// all falsely matched, causing theme:pets/theme:snow to over-match (out of
// 6882 photos library-wide, they matched 1306/1610 respectively). Fix: SQL
// first does a coarse pass with instr to shrink the candidate set (avoiding a
// full table scan; a substring hit is a necessary condition for a
// word-boundary hit, so nothing is missed), then does a precise pass on the
// Go side with a `\bkw\b` regex — captions are English text, so \b's word-
// character semantics are reliable.
func matchCaptionKeywords(ctx context.Context, db *sql.DB, keywords []string) ([]string, error) {
	if len(keywords) == 0 {
		return nil, nil
	}

	clauses := make([]string, len(keywords))
	args := make([]interface{}, len(keywords))
	boundaryRe := make([]*regexp.Regexp, len(keywords))
	for i, kw := range keywords {
		lower := strings.ToLower(kw)
		clauses[i] = "instr(lower(text), ?) > 0"
		args[i] = lower
		boundaryRe[i] = regexp.MustCompile(`\b` + regexp.QuoteMeta(lower) + `\b`)
	}
	q := `SELECT asset_id, lower(text) FROM asset_caption WHERE ` + strings.Join(clauses, " OR ")
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("moments: caption keyword query: %w", err)
	}
	defer rows.Close()

	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var id, text string
		if err := rows.Scan(&id, &text); err != nil {
			return nil, fmt.Errorf("moments: scan caption keyword hit: %w", err)
		}
		if seen[id] {
			continue // The same asset can have multiple caption rows; dedup avoids adding it to the result more than once.
		}
		for _, re := range boundaryRe {
			if re.MatchString(text) {
				seen[id] = true
				out = append(out, id)
				break
			}
		}
	}
	return out, rows.Err()
}

// loadThemeCandidatePool queries the theme engine's candidate pool:
// status='indexed', not trashed (deleted_at IS NULL AND offline=0), excludes
// documents (the negation of hasOcrExpr), excludes is_live_photo_video (the
// same criteria as the trip engine's loadTripCandidates, see the comment at
// the top of moments_trip.go — the reasoning applies equally to theme: a
// live photo's companion video shouldn't be counted as an independent photo
// among the theme members), and requires non-null taken_at (otherwise it
// can't be folded into the TimeFrom/TimeTo member time-range computation).
// Returns a map of id → taken_at, for BuildThemeMoments to intersect with the
// union of both paths' hits.
func loadThemeCandidatePool(ctx context.Context, db *sql.DB) (map[string]time.Time, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT a.id, a.taken_at
		FROM assets a
		WHERE a.status='indexed' AND a.deleted_at IS NULL AND a.offline=0
		  AND a.is_live_photo_video=0
		  AND a.taken_at IS NOT NULL
		  AND NOT (`+hasOcrExpr+`)`)
	if err != nil {
		return nil, fmt.Errorf("moments: theme candidate query: %w", err)
	}
	defer rows.Close()

	out := map[string]time.Time{}
	for rows.Next() {
		var id string
		var ts sql.NullString
		if err := rows.Scan(&id, &ts); err != nil {
			return nil, fmt.Errorf("moments: scan theme candidate: %w", err)
		}
		t := parseSQLiteTime(ts)
		if t == nil {
			continue // taken_at is already constrained non-null in the WHERE clause; this is just a belt-and-suspenders check.
		}
		out[id] = *t
	}
	return out, rows.Err()
}
