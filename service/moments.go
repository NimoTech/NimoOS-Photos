// The scheduling/assembly layer for Smart Moments: dispatches each recipe by
// recipe.Kind to the trip/theme engine to produce drafts → runs them through
// the shared curation step (PickFeaturedAndCover) to fill in featured/cover →
// persists idempotently (SyncRecipeMoments) → for kind=trip moments still on
// the template-fallback title (named_by_llm=0), tries LLM naming one by one
// (best-effort, silently skipped on failure, never blocks the main recompute
// flow). A theme moment's title is always recipe.Title (the curated name) and
// never enters the LLM naming loop — see the comment inside RecomputeAll.
package service

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// namer is the minimal interface for the LLM naming capability; the real
// implementation is pkg/aiclient.Client.Complete. Tests inject a fake so
// MomentsService doesn't directly depend on the HTTP/AI service.
type namer interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// momentsFailBackoff is the minimum interval StartScheduler waits before
// automatically retrying after RecomputeAll fails — same design as
// FaceService's clusterFailBackoff.
const momentsFailBackoff = 30 * time.Minute

// maxNamingCaptions is the cap on featured photo captions fed into the LLM
// naming prompt.
const maxNamingCaptions = 10

// maxLLMTitleWords is the word-count guard applied before persisting an LLM
// naming result: the prompt asks for "at most 4 words", but weaker local
// models don't reliably follow that constraint; this relaxes it to 6 words
// for some slack, and rejects the whole title (keeping the template name)
// past that — no truncation, since a half-cut long sentence is just as ugly
// as simply treating it as the model failing to give a satisfying answer.
const maxLLMTitleWords = 6

// MomentsService is the scheduling/assembly layer for Smart Moments.
type MomentsService struct {
	db           *sql.DB
	store        *MomentStore
	searcher     clipTextSearcher
	loadVec      clipVecLoader
	loadCover    coverImageLoader // Cover brightness-gate thumbnail loader; when nil, pickCover falls back directly to featured[0] (see SetLoadCover).
	namer        namer
	profileStore *ProfileStore // Moments M2 profile layer: the pet_entities/family engines write to user_profile_entities.
	reg          *TaskRegistry

	running atomic.Bool

	// Failure backoff: after the last RecomputeAll error, StartScheduler won't
	// trigger again for a while, avoiding a retry storm every minute (same as
	// FaceService).
	failMu      sync.Mutex
	nextAttempt time.Time
}

// NewMomentsService constructs a MomentsService. searcher/loadVec/namer are
// injected with production implementations by the caller (service.go
// NewService): searcher=SearchService (implements clipTextSearcher),
// loadVec=RealClipVecLoader(db), namer=aiclient.Client.
func NewMomentsService(db *sql.DB, store *MomentStore, searcher clipTextSearcher, loadVec clipVecLoader, namer namer) *MomentsService {
	return &MomentsService{
		db:           db,
		store:        store,
		searcher:     searcher,
		loadVec:      loadVec,
		namer:        namer,
		profileStore: NewProfileStore(db),
	}
}

// SetTaskRegistry injects a TaskRegistry so RecomputeAll can report progress.
func (s *MomentsService) SetTaskRegistry(reg *TaskRegistry) { s.reg = reg }

// SetLoadCover injects the thumbnail loader used by the cover brightness gate
// (production implementation: RealCoverImageLoader; wired up in service.go
// NewService). Same optional-injection pattern as SetThumbDir/
// SetTaskRegistry: if not called, loadCover stays nil and
// PickFeaturedAndCover falls back directly to featured[0] (equivalent to the
// behavior before this gate existed), so existing tests that don't wire up
// this gate are unaffected.
func (s *MomentsService) SetLoadCover(loadCover coverImageLoader) { s.loadCover = loadCover }

// Store exposes the underlying MomentStore so the route layer can read/write
// moments/recipes directly (listing, membership, recipe hot-reload) — these
// are pure repo-layer operations that don't need to go through the scheduling
// layer.
func (s *MomentsService) Store() *MomentStore { return s.store }

// RecomputeAll is the full-recompute entry point: for each enabled recipe,
// dispatches by kind to the engine to produce drafts, runs shared curation to
// fill in featured/cover, and persists idempotently; afterward, for moments
// still on the template-fallback title (named_by_llm=0), tries LLM naming one
// by one. CAS re-entrancy guard: returns nil immediately if a round is
// already running.
//
// A single recipe failing (engine query/persist etc.) only logs a Warn and
// skips it, moving on to the next recipe — same skip philosophy as the
// unknown-kind case: not calling SyncRecipeMoments means that recipe's moments
// from the previous round are left as-is in the store, never cleared. This
// isolation matters: the theme engine depends on CLIP semantic search (the
// immich ML container), and ML being offline is a routine transient state for
// this deployment; recipes are processed in key lexical order, and "theme:*"
// sorts before "trip" — if one recipe erroring aborted the whole round, ML
// going down would make even the ML-independent trip moments permanently
// unable to compute, replaying on every trigger. Only truly fatal
// infrastructure failures like ListRecipes/ListMoments (failing to read the
// recipe list itself) make RecomputeAll return an error overall.
// LLM naming is likewise best-effort: a single moment's naming failure is
// silently skipped.
func (s *MomentsService) RecomputeAll(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return nil
	}
	defer s.running.Store(false)

	taskID := fmt.Sprintf("moments_%d", time.Now().UnixNano())
	started := time.Now()
	// pub's Added field is repurposed under Type("moments") to mean "number of
	// recipes skipped this round" (same field FaceService uses for "faces
	// added" — the existing convention is each Type defines its own semantics
	// for it), carried only in the terminal state ("done"), so the frontend
	// can tell the user "some recipes were skipped due to ML being offline
	// etc." instead of the whole recompute reporting an error.
	pub := func(progress float64, status string, errKey string, errParams map[string]string, skipped int64) {
		if s.reg == nil {
			return
		}
		t := Task{
			ID:        taskID,
			Type:      "moments",
			Label:     "Organize moments",
			Progress:  progress,
			Status:    status,
			StartedAt: started,
		}
		if status == "done" {
			t.Added = skipped
		}
		if errKey != "" {
			t.SetError(errKey, errParams)
		}
		s.reg.Upsert(t)
	}
	pub(0, "running", "", nil, 0)

	recipes, err := s.store.ListRecipes(true)
	if err != nil {
		pub(0, "error", TaskErrMomentsRecomputeFailed, map[string]string{"detail": err.Error()}, 0)
		return fmt.Errorf("moments: list recipes: %w", err)
	}

	// petEntitiesProduced records whether this round's profile:pets
	// (kind=pet_entities) produced ≥1 personalized pet entity moment — if so,
	// when we reach theme:pets we call SyncRecipeMoments with empty drafts to
	// clear the concept version (the replacement rule; see design spec §2).
	// This replacement relies on recipes being processed in key lexical order
	// ("profile:pets" < "theme:pets", p<t, ListRecipes already sorts by key
	// ASC) — by the time we reach theme:pets, profile:pets is guaranteed to
	// have already run, so the flag is already set.
	var petEntitiesProduced bool
	var skipped int64
	for i, recipe := range recipes {
		if recipe.Key == "theme:pets" && petEntitiesProduced {
			// Personalized pet entities have been produced: replace the concept
			// version — call SyncRecipeMoments with empty drafts to clear
			// theme:pets's moments from the previous round, without running the
			// concept engine (this doesn't depend on BuildThemeMoments/CLIP; the
			// replacement itself shouldn't be affected by ML state).
			if err := s.store.SyncRecipeMoments(recipe.Key, nil); err != nil {
				zap.L().Warn("moments: failed to clear concept version theme:pets, skipping this round, keeping old moments",
					zap.String("key", recipe.Key), zap.Error(err))
				skipped++
			} else {
				zap.L().Info("moments: personalized pet entities produced, replacing concept version theme:pets (cleared)")
			}
			pub(0.7*float64(i+1)/float64(len(recipes)), "running", "", nil, 0)
			continue
		}

		draftCount, err := s.recomputeRecipe(ctx, recipe)
		if err != nil {
			// A single recipe failing only logs a Warn and moves on to the next:
			// not calling SyncRecipeMoments means that recipe's moments from the
			// previous round are left as-is, never cleared (see the comment at
			// the top of this method — an ML blip must not take down other
			// recipes, especially ML-independent trip).
			zap.L().Warn("moments: recipe recompute failed, skipping this round, keeping old moments",
				zap.String("key", recipe.Key), zap.Error(err))
			skipped++
			pub(0.7*float64(i+1)/float64(len(recipes)), "running", "", nil, 0)
			continue
		}
		if recipe.Kind == "pet_entities" && draftCount > 0 {
			petEntitiesProduced = true
		}
		pub(0.7*float64(i+1)/float64(len(recipes)), "running", "", nil, 0)
	}

	// LLM best-effort naming: only pick moments produced by kind=trip recipes
	// that are still on the template-fallback title (named_by_llm=0) this
	// round — the store already guarantees rows with named_by_llm=1 won't have
	// their title overwritten by SyncRecipeMoments above; this just avoids
	// calling the LLM again for them.
	//
	// theme moments never enter the LLM naming loop: a theme's title is
	// recipe.Title, a name curated by the ops team (e.g. "Pet Moments"); real-
	// device testing found local weak models would mangle it (turning "pets"
	// into "Sunset on Highway"), pure sabotage — trip moments don't have this
	// "already has a good name" luxury (the template title is just a
	// "place + Trip" fallback), which is why they need the LLM to come up with
	// something more vivid.
	recipeKind := make(map[string]string, len(recipes))
	for _, r := range recipes {
		recipeKind[r.Key] = r.Kind
	}
	moments, err := s.store.ListMoments()
	if err != nil {
		pub(0.7, "error", TaskErrMomentsRecomputeFailed, map[string]string{"detail": err.Error()}, 0)
		return fmt.Errorf("moments: list moments: %w", err)
	}
	var toName []Moment
	for _, m := range moments {
		if m.NamedByLLM {
			continue
		}
		if recipeKind[m.RecipeKey] != "trip" {
			continue
		}
		toName = append(toName, m)
	}
	for i, m := range toName {
		s.tryNameMoment(ctx, m)
		pub(0.7+0.3*float64(i+1)/float64(len(toName)), "running", "", nil, 0)
	}

	pub(1, "done", "", nil, skipped)
	go func() {
		time.Sleep(taskCleanupDelay)
		if s.reg != nil {
			s.reg.Remove(taskID)
		}
	}()
	return nil
}

// recomputeRecipe processes a single recipe: engine produces drafts → shared
// curation fills in featured/cover → persists idempotently. Returns the
// number of drafts produced this round (RecomputeAll uses it to tell whether
// pet_entities produced ≥1 entity moment, to decide the theme:pets
// replacement rule).
func (s *MomentsService) recomputeRecipe(ctx context.Context, recipe MomentRecipe) (int, error) {
	var drafts []MomentDraft
	var err error
	switch recipe.Kind {
	case "trip":
		drafts, err = BuildTripMoments(ctx, s.db, recipe)
	case "theme":
		drafts, err = BuildThemeMoments(ctx, s.db, s.searcher, recipe)
	case "pet_entities":
		drafts, err = BuildPetEntityMoments(ctx, s.db, s.searcher, s.profileStore, s.store, recipe)
	case "family":
		drafts, err = BuildFamilyMoments(ctx, s.db, s.profileStore, recipe)
	default:
		// Unknown kind: hot-reloaded recipe data may introduce an algorithm not
		// yet implemented; skip rather than error, so it doesn't block other
		// recipes' recompute.
		zap.L().Warn("moments: unknown recipe kind, skipping", zap.String("key", recipe.Key), zap.String("kind", recipe.Kind))
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	params, err := ParseParams(recipe)
	if err != nil {
		return 0, err
	}

	for i := range drafts {
		featured, cover, err := PickFeaturedAndCover(ctx, s.db, drafts[i].Assets, params.MaxFeatured, s.loadVec, s.loadCover)
		if err != nil {
			return 0, err
		}
		featuredSet := make(map[string]bool, len(featured))
		for _, id := range featured {
			featuredSet[id] = true
		}
		for j := range drafts[i].Assets {
			drafts[i].Assets[j].Featured = featuredSet[drafts[i].Assets[j].AssetID]
		}
		drafts[i].CoverAssetID = cover
	}

	if err := s.store.SyncRecipeMoments(recipe.Key, drafts); err != nil {
		return 0, err
	}
	return len(drafts), nil
}

// tryNameMoment is a single best-effort attempt at LLM naming: failure to
// read featured captions, LLM call failure/timeout, or an empty returned
// title are all silently skipped (only logging a Warn) — no error propagates
// upward, since the caller RecomputeAll doesn't check the return value for
// any moment's naming result.
func (s *MomentsService) tryNameMoment(ctx context.Context, m Moment) {
	captions, err := s.featuredCaptions(ctx, m.ID)
	if err != nil {
		zap.L().Warn("moments: failed to read featured captions, skipping LLM naming",
			zap.String("moment_id", m.ID), zap.Error(err))
		return
	}

	title, err := s.namer.Complete(ctx, buildNamingPrompt(m, captions))
	if err != nil {
		zap.L().Warn("moments: LLM naming failed, skipping", zap.String("moment_id", m.ID), zap.Error(err))
		return
	}
	title = cleanLLMTitle(title)
	if title == "" {
		return
	}
	// Word-count guard: the prompt asks for "at most 4 words", but weaker local
	// models don't reliably follow that constraint (real-device testing
	// observed 5 trips pinned to long sentences, e.g.
	// "May 28, 2011 - ...Overcast Sky"). The semantics shift from "trust the
	// model to obey" to "accept only if it qualifies, otherwise treat it as
	// nothing was said" — a title over 6 words (a bit of slack rather than
	// strictly enforcing 4) is rejected outright, keeping the original
	// template name, only logged and never persisted.
	if wc := len(strings.Fields(title)); wc > maxLLMTitleWords {
		zap.L().Debug("moments: LLM title exceeded word limit, rejected, keeping template name",
			zap.String("moment_id", m.ID), zap.String("rejected_title", title), zap.Int("word_count", wc))
		return
	}
	if err := s.store.SetMomentTitle(m.ID, title); err != nil {
		zap.L().Warn("moments: failed to persist LLM title", zap.String("moment_id", m.ID), zap.Error(err))
	}
}

// featuredCaptions returns the caption text for a moment's featured members
// (sorted by score descending), up to maxNamingCaptions entries; a member
// with no caption (Parser not deployed / not backfilled yet) is simply
// skipped for that asset, not treated as an error.
func (s *MomentsService) featuredCaptions(ctx context.Context, momentID string) ([]string, error) {
	assets, err := s.store.GetMomentAssets(momentID, true)
	if err != nil {
		return nil, fmt.Errorf("moments: get featured assets: %w", err)
	}
	if len(assets) == 0 {
		return nil, nil
	}
	if len(assets) > maxNamingCaptions {
		assets = assets[:maxNamingCaptions]
	}

	ids := make([]string, len(assets))
	for i, a := range assets {
		ids[i] = a.AssetID
	}
	placeholders := strings.Repeat("?,", len(ids)-1) + "?"
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT text FROM asset_caption WHERE asset_id IN (`+placeholders+`) AND text != ''`, args...)
	if err != nil {
		return nil, fmt.Errorf("moments: query captions: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, fmt.Errorf("moments: scan caption: %w", err)
		}
		out = append(out, text)
	}
	return out, rows.Err()
}

// buildNamingPrompt assembles the naming prompt fed to the LLM: time/place
// info + featured photo captions (up to maxNamingCaptions) + few-shot
// examples, asking the model to reply with a title that is "Title Case, at
// most 4 words, English only, no punctuation or quotes".
//
// Every issue exposed by real-device testing is hardened here:
//   - removed the old "for a personal photo app" wording — weak local models
//     would echo it verbatim into the title (e.g. "Nighttime Las Vegas Photo
//     App."), so the whole prompt now avoids the phrase "photo app" entirely;
//   - added few-shot examples + "do not repeat or explain these instructions",
//     reducing the chance the model copies the instructions themselves as the
//     answer;
//   - explicitly requires English only (no other languages), to counter output
//     mixing in Chinese that exceeds 4 words;
//   - explicitly requires Title Case, no punctuation/quotes, reducing the
//     amount of fallback cleanup cleanLLMTitle needs to do.
func buildNamingPrompt(m Moment, captions []string) string {
	var b strings.Builder
	b.WriteString("You are naming a moment: a curated group of related personal photos.\n")
	if !m.TimeFrom.IsZero() {
		if !m.TimeTo.IsZero() && !m.TimeTo.Equal(m.TimeFrom) {
			fmt.Fprintf(&b, "Time range: %s to %s\n", m.TimeFrom.Format("Jan 2, 2006"), m.TimeTo.Format("Jan 2, 2006"))
		} else {
			fmt.Fprintf(&b, "Time: %s\n", m.TimeFrom.Format("Jan 2, 2006"))
		}
	}
	if m.Place != "" {
		b.WriteString("Place: " + m.Place + "\n")
	}
	if len(captions) > 0 {
		b.WriteString("Photo descriptions:\n")
		for _, c := range captions {
			b.WriteString("- " + c + "\n")
		}
	}
	b.WriteString("Examples:\n")
	b.WriteString("Photos: sunset over golden gate bridge, san francisco skyline at dusk -> Golden Gate Evenings\n")
	b.WriteString("Photos: skiing in the alps, snowy mountain slopes -> Alpine Ski Days\n")
	b.WriteString("Reply with ONLY the title, nothing else. Requirements: Title Case; English only (no other languages); at most 4 words; no punctuation or quotes; do not repeat or explain these instructions.")
	return b.String()
}

// maxLLMTitleRunes is the hard truncation cap before persisting an LLM naming
// result: the prompt asks for "at most 4 words", but the model doesn't
// guarantee compliance (cloud models in particular occasionally ignore the
// instruction and tack on a long explanation). Without this safety net, a
// runaway long text would go straight into moments.title and be shown to the
// user; truncation adds no ellipsis — for a title, a plain cut is fine, no
// need to signal "this was truncated".
const maxLLMTitleRunes = 80

// cleanLLMTitle cleans up the LLM output: trims leading/trailing whitespace,
// strips one layer of wrapping quotes, keeps only the first line (guarding
// against the model tacking on explanatory text despite instructions), and
// finally does a rune-safe truncation to maxLLMTitleRunes, to guard against
// the model ignoring the "at most 4 words" constraint and dumping long text
// straight into the store.
func cleanLLMTitle(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	s = strings.Trim(s, `"'`)
	s = strings.TrimSpace(s)
	if runes := []rune(s); len(runes) > maxLLMTitleRunes {
		s = string(runes[:maxLLMTitleRunes])
	}
	return s
}

// StartScheduler runs a background goroutine that triggers RecomputeAll:
//   - once per day at 04:xx (minute < 5), offset from FaceService's 03:xx window;
//   - failure backoff: won't automatically retry within momentsFailBackoff after an error.
//
// Same pattern as FaceService.StartScheduler (faces.go:1042-1097);
// RecomputeAll's own CAS already prevents concurrent re-entrancy with the
// immediate trigger hung off SetOnBatchDone, so no extra locking is needed
// here.
func (s *MomentsService) StartScheduler(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				s.failMu.Lock()
				nextOK := s.nextAttempt
				s.failMu.Unlock()
				if !nextOK.IsZero() && t.Before(nextOK) {
					continue
				}

				if t.Hour() != 4 || t.Minute() >= 5 {
					continue
				}

				if err := s.RecomputeAll(ctx); err != nil {
					zap.L().Error("moments recompute failed", zap.Error(err))
					s.failMu.Lock()
					s.nextAttempt = time.Now().Add(momentsFailBackoff)
					s.failMu.Unlock()
				} else {
					s.failMu.Lock()
					s.nextAttempt = time.Time{}
					s.failMu.Unlock()
				}
			}
		}
	}()
}
