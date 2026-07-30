package service

import (
	"sync"
	"time"
)

// defaultBackfillGateInterval 是每条重补跑链的最小间隔。
//
// 为什么需要节流:重补跑链(CLIP/OCR/doc/美学/caption 补跑、sprite 补跑)挂在
// 两个高频触发源上——批次完成钩子(ingestTracker 判定「队列空闲 6 秒」即算一
// 批,见 defaultIngestIdleTimeout)和 ML 就绪跳变。一块持续变动的大容量素材盘
// 会让批次每几秒完成一次,于是整条链每几秒重跑一次、每轮都遍历全部欠账资产。
// 生产实测后果:磁盘 24 小时满速顺序读而进度零推进。
//
// 10 分钟的取舍:配合 leading-edge 立即执行,用户上传后的第一轮补跑没有任何
// 延迟(体感不变);只有「窗口内的连串触发」才被合并到窗口末尾跑一轮。
const defaultBackfillGateInterval = 10 * time.Minute

// backfillGate 是重补跑链的节流闸。语义:**每条链各自**一个窗口,
// leading-edge 立即同步执行,窗口内的后续触发既不丢弃也不各自执行,而是合并
// 成窗口末尾的一轮。
//
// 不用「简单最小间隔 + 丢弃」是因为触发会携带真实欠账(刚上传的资产、刚插回
// 的盘),丢掉就等于欠账要等下一次偶然触发才被处理;合并到窗口末尾既限流又不
// 丢事件。
//
// 窗口按 name 各自独立而不是全闸共用一个:多条链共用一个窗口时,谁先触发就吃
// 掉 leading edge,别的链哪怕是第一次触发也要等到窗口末尾——实测表现是服务启动
// 时 ML 恢复链吃掉 leading edge,紧随其后用户上传的那一批补跑被推迟整整一个
// 窗口。各链独立后,每条链都保有「安静期后首次触发立即跑」的体感,同时各自被
// 限到每窗口一轮。
type backfillGate struct {
	mu      sync.Mutex
	min     time.Duration
	chains  map[string]*gatedChain
	now     func() time.Time
	afterFn func(time.Duration, func()) *time.Timer
}

// gatedChain 是单条链的节流状态。
type gatedChain struct {
	lastRun   time.Time
	pendingFn func()
	timer     *time.Timer
}

func newBackfillGate(min time.Duration) *backfillGate {
	return &backfillGate{
		min:     min,
		chains:  map[string]*gatedChain{},
		now:     time.Now,
		afterFn: time.AfterFunc,
	}
}

// Run 按节流语义执行名为 name 的链。调用方通常已经在自己的 goroutine 里
// (批次钩子/恢复链都是 `go ...`),所以 leading edge 直接同步执行,不再多开
// 一层 goroutine。窗口内的多次触发只保留最后一个 fn——同名即同一条链,跑一轮
// 等价。
func (g *backfillGate) Run(name string, fn func()) {
	g.mu.Lock()
	c := g.chains[name]
	if c == nil {
		c = &gatedChain{}
		g.chains[name] = c
	}
	now := g.now()
	if !c.lastRun.IsZero() && now.Sub(c.lastRun) < g.min {
		c.pendingFn = fn
		if c.timer == nil {
			c.timer = g.afterFn(g.min-now.Sub(c.lastRun), func() { g.firePending(name) })
		}
		g.mu.Unlock()
		return
	}
	c.lastRun = now
	g.mu.Unlock()
	fn()
}

// firePending 是某条链窗口末尾的合并执行。
func (g *backfillGate) firePending(name string) {
	g.mu.Lock()
	c := g.chains[name]
	if c == nil {
		g.mu.Unlock()
		return
	}
	fn := c.pendingFn
	c.pendingFn = nil
	c.timer = nil
	c.lastRun = g.now()
	g.mu.Unlock()
	if fn != nil {
		fn()
	}
}
