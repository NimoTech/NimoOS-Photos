// Smart Moments 的调度装配层:按 recipe.Kind 分派 trip/theme 两个引擎产出
// 草稿 → 过共用选优(PickFeaturedAndCover)填精选/封面 → 幂等落库
// (SyncRecipeMoments)→ 对模板打底(named_by_llm=0)的时刻逐个尝试 LLM 命名
// (best-effort,失败静默跳过,绝不阻塞重算主流程)。
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

// namer 是 LLM 命名能力的最小接口,真实现见 pkg/aiclient.Client.Complete;
// 测试注入 fake,避免 MomentsService 直接依赖 HTTP/AI 服务。
type namer interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// momentsFailBackoff 是 RecomputeAll 失败后 StartScheduler 短期内不再自动
// 尝试的最短间隔,与 FaceService 的 clusterFailBackoff 同款设计。
const momentsFailBackoff = 30 * time.Minute

// maxNamingCaptions 是喂给 LLM 命名 prompt 的精选照片 caption 上限。
const maxNamingCaptions = 10

// MomentsService 是 Smart Moments 的调度装配层。
type MomentsService struct {
	db       *sql.DB
	store    *MomentStore
	searcher clipTextSearcher
	loadVec  clipVecLoader
	namer    namer
	reg      *TaskRegistry

	running atomic.Bool

	// 失败 backoff:上次 RecomputeAll 出错后短期内 StartScheduler 不再触发,
	// 避免每分钟重试风暴(与 FaceService 同款)。
	failMu      sync.Mutex
	nextAttempt time.Time
}

// NewMomentsService 构造 MomentsService。searcher/loadVec/namer 由调用方
// (service.go NewService)注入生产实现:searcher=SearchService(实现
// clipTextSearcher)、loadVec=RealClipVecLoader(db)、namer=aiclient.Client。
func NewMomentsService(db *sql.DB, store *MomentStore, searcher clipTextSearcher, loadVec clipVecLoader, namer namer) *MomentsService {
	return &MomentsService{
		db:       db,
		store:    store,
		searcher: searcher,
		loadVec:  loadVec,
		namer:    namer,
	}
}

// SetTaskRegistry injects a TaskRegistry so RecomputeAll can report progress.
func (s *MomentsService) SetTaskRegistry(reg *TaskRegistry) { s.reg = reg }

// Store 暴露底层 MomentStore,供 route 层直接读写 moments/recipes(列表、
// 成员、recipe 热更新)——这些是纯 repo 层操作,不需要经过调度层。
func (s *MomentsService) Store() *MomentStore { return s.store }

// RecomputeAll 是全量重算入口:对每个 enabled recipe 按 kind 分派引擎产出
// 草稿、过共用选优填精选/封面、幂等落库,随后对本轮仍是模板打底
// (named_by_llm=0)的时刻逐个尝试 LLM 命名。CAS 防重入:已有一轮在跑时
// 直接返回 nil。除 LLM 命名外的失败(引擎查询/落库等基础设施故障)会中断
// 本轮并向上返回 error;LLM 命名本身是 best-effort,单个时刻命名失败只
// 静默跳过,绝不影响本方法的返回值。
func (s *MomentsService) RecomputeAll(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return nil
	}
	defer s.running.Store(false)

	taskID := fmt.Sprintf("moments_%d", time.Now().UnixNano())
	started := time.Now()
	pub := func(progress float64, status string, errKey string, errParams map[string]string) {
		if s.reg == nil {
			return
		}
		t := Task{
			ID:        taskID,
			Type:      "moments",
			Label:     "整理时刻",
			Progress:  progress,
			Status:    status,
			StartedAt: started,
		}
		if errKey != "" {
			t.SetError(errKey, errParams)
		}
		s.reg.Upsert(t)
	}
	pub(0, "running", "", nil)

	recipes, err := s.store.ListRecipes(true)
	if err != nil {
		pub(0, "error", TaskErrMomentsRecomputeFailed, map[string]string{"detail": err.Error()})
		return fmt.Errorf("moments: list recipes: %w", err)
	}

	for i, recipe := range recipes {
		if err := s.recomputeRecipe(ctx, recipe); err != nil {
			pub(float64(i)/float64(len(recipes)), "error", TaskErrMomentsRecomputeFailed, map[string]string{"detail": err.Error()})
			return fmt.Errorf("moments: recompute recipe %q: %w", recipe.Key, err)
		}
		pub(0.7*float64(i+1)/float64(len(recipes)), "running", "", nil)
	}

	// LLM best-effort 命名:只挑本轮仍是模板打底(named_by_llm=0)的时刻,
	// store 已保证 named_by_llm=1 的行不会被上面的 SyncRecipeMoments 覆盖
	// title,这里只是不去重复调用 LLM。
	moments, err := s.store.ListMoments()
	if err != nil {
		pub(0.7, "error", TaskErrMomentsRecomputeFailed, map[string]string{"detail": err.Error()})
		return fmt.Errorf("moments: list moments: %w", err)
	}
	var toName []Moment
	for _, m := range moments {
		if !m.NamedByLLM {
			toName = append(toName, m)
		}
	}
	for i, m := range toName {
		s.tryNameMoment(ctx, m)
		pub(0.7+0.3*float64(i+1)/float64(len(toName)), "running", "", nil)
	}

	pub(1, "done", "", nil)
	go func() {
		time.Sleep(taskCleanupDelay)
		if s.reg != nil {
			s.reg.Remove(taskID)
		}
	}()
	return nil
}

// recomputeRecipe 处理单个 recipe:引擎产出草稿 → 共用选优填精选/封面 →
// 幂等落库。
func (s *MomentsService) recomputeRecipe(ctx context.Context, recipe MomentRecipe) error {
	var drafts []MomentDraft
	var err error
	switch recipe.Kind {
	case "trip":
		drafts, err = BuildTripMoments(ctx, s.db, recipe)
	case "theme":
		drafts, err = BuildThemeMoments(ctx, s.db, s.searcher, recipe)
	default:
		// 未知 kind:热更新的 recipe 数据可能引入未来才实现的算法,跳过而非
		// 报错,不阻塞其它 recipe 的重算。
		zap.L().Warn("moments: 未知 recipe kind,跳过", zap.String("key", recipe.Key), zap.String("kind", recipe.Kind))
		return nil
	}
	if err != nil {
		return err
	}

	params, err := ParseParams(recipe)
	if err != nil {
		return err
	}

	for i := range drafts {
		featured, cover, err := PickFeaturedAndCover(ctx, s.db, drafts[i].Assets, params.MaxFeatured, s.loadVec)
		if err != nil {
			return err
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

	return s.store.SyncRecipeMoments(recipe.Key, drafts)
}

// tryNameMoment 是 LLM 命名的 best-effort 单点尝试:读精选 caption 失败、
// LLM 调用失败/超时、返回空标题,一律静默跳过(仅 Warn 留痕),不向上传播
// error——调用方 RecomputeAll 对所有时刻的命名结果都不检查返回值。
func (s *MomentsService) tryNameMoment(ctx context.Context, m Moment) {
	captions, err := s.featuredCaptions(ctx, m.ID)
	if err != nil {
		zap.L().Warn("moments: 读取精选 caption 失败,跳过 LLM 命名",
			zap.String("moment_id", m.ID), zap.Error(err))
		return
	}

	title, err := s.namer.Complete(ctx, buildNamingPrompt(m, captions))
	if err != nil {
		zap.L().Warn("moments: LLM 命名失败,跳过", zap.String("moment_id", m.ID), zap.Error(err))
		return
	}
	title = cleanLLMTitle(title)
	if title == "" {
		return
	}
	if err := s.store.SetMomentTitle(m.ID, title); err != nil {
		zap.L().Warn("moments: 落库 LLM 标题失败", zap.String("moment_id", m.ID), zap.Error(err))
	}
}

// featuredCaptions 返回某时刻精选成员(按 score 降序)至多 maxNamingCaptions
// 条的 caption 文本;成员没有 caption(Parser 未部署/尚未回流)时对应资产
// 直接跳过,不算错误。
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

// buildNamingPrompt 拼装喂给 LLM 的命名 prompt:时间/地点信息 + 精选照片
// caption(至多 maxNamingCaptions 条),要求模型回一个"至多 4 个单词的英文
// 标题"。
func buildNamingPrompt(m Moment, captions []string) string {
	var b strings.Builder
	b.WriteString("You are naming a moment (a group of related photos) for a personal photo app.\n")
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
	b.WriteString("Give a short title of at most 4 words, English, no quotes.")
	return b.String()
}

// maxLLMTitleRunes 是 LLM 命名结果落库前的硬截断上限:prompt 要求"至多 4
// 个单词",但模型不保证守约束(尤其云端模型偶尔会无视指令附赠解释性长文)。
// 没有这道防线,失控的长文本会原样进 moments.title 展示给用户,截断不加
// 省略号——标题场景直接截断即可,不必刻意提示"这里被截过"。
const maxLLMTitleRunes = 80

// cleanLLMTitle 清洗 LLM 输出:掐头去尾空白、去掉一层包裹引号、只取第一行
// (防止模型无视指令附赠解释性文字),最后按 rune 安全截断到
// maxLLMTitleRunes,防止模型不守"至多 4 个单词"的约束时把长文原样落库。
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
//   - once per day at 04:xx(minute < 5),错开 FaceService 的 03:xx 窗口;
//   - 失败 backoff:出错后 momentsFailBackoff 内不再自动尝试。
//
// 照 FaceService.StartScheduler(faces.go:1042-1097)同款范式;RecomputeAll
// 自身的 CAS 已防止与 SetOnBatchDone 挂的即时触发并发重入,这里不需要
// 额外加锁。
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
