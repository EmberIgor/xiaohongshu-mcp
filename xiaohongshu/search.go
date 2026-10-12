package xiaohongshu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
)

type SearchResult struct {
	Search struct {
		Feeds FeedsValue `json:"feeds"`
	} `json:"search"`
}

// FilterOption 筛选选项结构体
type FilterOption struct {
	SortBy      string `json:"sort_by,omitempty" jsonschema:"排序依据: 综合|最新|最多点赞|最多评论|最多收藏,默认为'综合'"`
	NoteType    string `json:"note_type,omitempty" jsonschema:"笔记类型: 不限|视频|图文,默认为'不限'"`
	PublishTime string `json:"publish_time,omitempty" jsonschema:"发布时间: 不限|一天内|一周内|半年内,默认为'不限'"`
	SearchScope string `json:"search_scope,omitempty" jsonschema:"搜索范围: 不限|已看过|未看过|已关注,默认为'不限'"`
	Location    string `json:"location,omitempty" jsonschema:"位置距离: 不限|同城|附近,默认为'不限'"`
}

// filterGroup 面板上的一个筛选组：标签是什么、对应入参的哪个字段、允许哪些取值。
//
// 组和选项一律按文本定位，不用序号。面板里同一个选项可能渲染成多个 div.tags
// （数量随视口而变），首项是否重复各组也不一致，下标对不齐。
type filterGroup struct {
	label        string                    // 面板上这一组的标签文本
	defaultValue string                    // 页面初始选中的值，不需要重复点击
	pick         func(FilterOption) string // 从入参里取这一组的值
	allowed      []string                  // 合法取值；在打开页面之前就能挡掉写错的值
}

var filterGroups = []filterGroup{
	{"排序依据", "综合", func(f FilterOption) string { return f.SortBy },
		[]string{"综合", "最新", "最多点赞", "最多评论", "最多收藏"}},
	{"笔记类型", "不限", func(f FilterOption) string { return f.NoteType },
		[]string{"不限", "视频", "图文"}},
	{"发布时间", "不限", func(f FilterOption) string { return f.PublishTime },
		[]string{"不限", "一天内", "一周内", "半年内"}},
	{"搜索范围", "不限", func(f FilterOption) string { return f.SearchScope },
		[]string{"不限", "已看过", "未看过", "已关注"}},
	{"位置距离", "不限", func(f FilterOption) string { return f.Location },
		[]string{"不限", "同城", "附近"}},
}

// pendingFilter 一个待应用的筛选项。
type pendingFilter struct {
	group  string // 组标签
	option string // 选项文本
}

// collectFilters 把入参整理成待应用的筛选项，同一组以最后一个非空值为准。
//
// 校验放在这里是为了在打开浏览器之前就挡掉写错的值——否则要等导航、悬停、
// 在面板里找不到之后才能报错，等于为了说一句"你写错了"先向平台发一次请求。
func collectFilters(filters []FilterOption) ([]pendingFilter, error) {
	selected := make(map[string]string, len(filterGroups))

	for _, f := range filters {
		for _, g := range filterGroups {
			value := g.pick(f)
			if value == "" {
				continue
			}
			if !slices.Contains(g.allowed, value) {
				return nil, fmt.Errorf("%s 不支持 %q，可选：%s",
					g.label, value, strings.Join(g.allowed, "、"))
			}
			selected[g.label] = value
		}
	}

	var pending []pendingFilter
	for _, g := range filterGroups {
		value, ok := selected[g.label]
		if !ok || value == g.defaultValue {
			continue
		}
		pending = append(pending, pendingFilter{group: g.label, option: value})
	}

	return pending, nil
}

func ValidateSearchFilters(filters ...FilterOption) error {
	_, err := collectFilters(filters)
	return err
}

type SearchAction struct {
	page *rod.Page
}

type SearchScrollConfig struct {
	MaxScrolls int
	Limit      int
}

func ValidateSearchOptions(maxScrolls int, limit *int) error {
	if maxScrolls < 0 || maxScrolls > 3 {
		return fmt.Errorf("max_scrolls must be between 0 and 3")
	}
	if limit != nil && (*limit < 1 || *limit > 200) {
		return fmt.Errorf("limit must be between 1 and 200")
	}
	return nil
}

type rodSearchSource struct {
	page             *rod.Page
	stateRead        bool
	skipInitialState bool
	// primed 保存「首批接受边界」已经消费式读到的数据。read() 首次被调用时把它并回
	// 返回值，保证判定阶段读到的数据一定进入采集器：不重复、不丢失（SPEC §6）。
	primed     []Feed
	primedUsed bool
}

func (r rodSearchSource) signal() error { return checkSearchSignals(r.page) }
func (r rodSearchSource) scroll() error {
	_, e := r.page.Eval(`() => window.scrollTo(0,document.body.scrollHeight)`)
	if e != nil {
		return fmt.Errorf("scroll search results: %w", e)
	}
	return nil
}
func (r rodSearchSource) wait(ctx context.Context, d time.Duration) error {
	return waitSearchDelay(ctx, d)
}

// searchSnapshotJSTemplate 生成「读一次搜索页快照」的 JS。%s 是消费方式：采集用
// splice(0) 取走被动捕获队列，探针用 slice(0) 只读。两种用途必须共用同一份来源与
// 选择规则，只差这一个词，否则「就绪判定」与「实际采集」会再次分家（SPEC §6）。
const searchSnapshotJSTemplate = `() => {
	const s = window.__INITIAL_STATE__;
	let f = s?.search?.feeds;
	if (f && f.value !== undefined) f = f.value; else if (f && f._value !== undefined) f = f._value;
	const batches = window.__xhsSearchBatches || [];
	const captured = batches.%s(0);
	return JSON.stringify({
		state: Array.isArray(f) ? f : [],
		batches: captured,
		hasInitialState: window.__INITIAL_STATE__ !== undefined,
		domNotes: document.querySelectorAll('#noteContainer .note-item, section.note-item').length,
	});
}`

var (
	searchConsumeSnapshotJS = fmt.Sprintf(searchSnapshotJSTemplate, "splice")
	searchProbeSnapshotJS   = fmt.Sprintf(searchSnapshotJSTemplate, "slice")
)

type searchSnapshot struct {
	State           json.RawMessage   `json:"state"`
	Batches         []json.RawMessage `json:"batches"`
	HasInitialState bool              `json:"hasInitialState"`
	DOMNotes        int               `json:"domNotes"`
}

func readSearchSnapshot(page *rod.Page, js string) (searchSnapshot, error) {
	v, err := page.Eval(js)
	if err != nil {
		return searchSnapshot{}, err
	}
	var snap searchSnapshot
	if err := json.Unmarshal([]byte(v.Value.Str()), &snap); err != nil {
		return searchSnapshot{}, err
	}
	return snap, nil
}

// decode 用与采集完全相同的规则把快照解成「可采集笔记」：被动捕获批次优先（有批次
// 就不用可能过期的 IS 快照），没有批次时才读 IS。useInitialState=false（已读过、
// 或筛选后）时只认批次。非 note、无 id 一律不算。
func (s searchSnapshot) decode(useInitialState bool) ([]Feed, error) {
	decodeOne := func(raw []byte) ([]Feed, error) {
		if len(raw) > 0 && raw[0] == '{' {
			return normalizeSearchItems(raw)
		}
		var fs []Feed
		er := json.Unmarshal(raw, &fs)
		if er != nil {
			return nil, er
		}
		notes := make([]Feed, 0, len(fs))
		for _, f := range fs {
			if isCollectableNote(f) {
				notes = append(notes, f)
			}
		}
		return notes, nil
	}
	var out []Feed
	if useInitialState && len(s.Batches) == 0 && len(s.State) > 0 {
		fs, err := decodeOne(s.State)
		if err != nil {
			return nil, err
		}
		out = append(out, fs...)
	}
	for _, raw := range s.Batches {
		fs, err := normalizeSearchItems(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, fs...)
	}
	return out, nil
}

func (r *rodSearchSource) read() ([]Feed, error) {
	snap, err := readSearchSnapshot(r.page, searchConsumeSnapshotJS)
	if err != nil {
		return nil, fmt.Errorf("read search feed state: %w", err)
	}
	useInitialState := !r.stateRead && !r.skipInitialState
	// 快照已经把页面上的捕获队列取走，无论解码成败都不能再读第二遍 IS。
	r.stateRead = true
	out, err := snap.decode(useInitialState)
	if err != nil {
		return nil, err
	}
	if !r.primedUsed && len(r.primed) > 0 {
		out = append(r.primed, out...)
		r.primedUsed = true
	}
	return out, nil
}

// probe 只读不消费地看一眼「现在能不能采到东西」。它既不改 stateRead，也不取走捕获
// 队列，否则正式采集就读不到同一批数据（SPEC §6）。
func (r *rodSearchSource) probe() (searchProbe, error) {
	snap, err := readSearchSnapshot(r.page, searchProbeSnapshotJS)
	if err != nil {
		return searchProbe{}, err
	}
	feeds, err := snap.decode(!r.stateRead && !r.skipInitialState)
	if err != nil {
		return searchProbe{}, err
	}
	return searchProbe{
		Collectable:     len(feeds),
		HasInitialState: snap.HasInitialState,
		Batches:         len(snap.Batches),
		DOMNotes:        snap.DOMNotes,
	}, nil
}

func waitSearchDelay(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func checkSearchSignals(page *rod.Page) error {
	v, err := page.Eval(`() => { const visible=e=>{if(!e)return false;const s=getComputedStyle(e),r=e.getBoundingClientRect();return s.display!=='none'&&s.visibility!=='hidden'&&Number(s.opacity)!==0&&r.width>0&&r.height>0}; const risk=[...document.querySelectorAll('[class*=captcha],[class*=verify],[class*=risk-control],[class*=riskControl]')].some(visible); const login=[...document.querySelectorAll('[class*=login-modal],[class*=loginModal],[class*=login_dialog]')].some(visible); return {url:location.pathname,risk,login}; }`)
	if err != nil {
		return fmt.Errorf("inspect search risk/login state: %w", err)
	}
	var state struct {
		URL   string `json:"url"`
		Risk  bool   `json:"risk"`
		Login bool   `json:"login"`
	}
	if err = v.Value.Unmarshal(&state); err != nil {
		return fmt.Errorf("decode search risk/login state: %w", err)
	}
	if state.Risk {
		return fmt.Errorf("risk_control: visible verification challenge detected")
	}
	if state.Login || (strings.Contains(state.URL, "/login") && (strings.HasSuffix(state.URL, "/login") || strings.Contains(state.URL, "/login/"))) {
		return fmt.Errorf("login_required: login dialog or redirect detected")
	}
	return nil
}

func NewSearchAction(page *rod.Page) *SearchAction {
	pp := page.Timeout(60 * time.Second)

	return &SearchAction{page: pp}
}

func (s *SearchAction) Search(ctx context.Context, keyword string, filters ...FilterOption) ([]Feed, error) {
	r, err := s.SearchWithConfig(ctx, keyword, SearchScrollConfig{}, filters...)
	return r.Feeds, err
}

func (s *SearchAction) SearchWithConfig(ctx context.Context, keyword string, cfg SearchScrollConfig, filters ...FilterOption) (SearchOutcome, error) {
	if err := ctx.Err(); err != nil {
		return SearchOutcome{}, err
	}
	// 先校验筛选取值，必须在导航之前——写错的值不该先向平台发一次请求再报错。
	pending, err := collectFilters(filters)
	if err != nil {
		return SearchOutcome{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	// 注意 .Context(ctx) 会替换掉 NewSearchAction 里设的 60s deadline，必须在其后重新 Timeout，
	// 否则搜索页不 stable 时 MustWaitStable/MustWait 会永久挂起（无 deadline 可依赖）。
	page := s.page.Context(ctx).Timeout(90 * time.Second)
	if _, err := page.EvalOnNewDocument(searchCaptureHook); err != nil {
		return SearchOutcome{}, fmt.Errorf("install search capture: %w", err)
	}

	searchURL := makeSearchURL(keyword)
	if err := page.Navigate(searchURL); err != nil {
		return SearchOutcome{}, fmt.Errorf("search navigation failed: %w", err)
	}
	if err := waitSearchReady(ctx, page); err != nil {
		return SearchOutcome{}, err
	}
	humanize.Delay(ctx, humanize.AfterNavigate)
	if err := ctx.Err(); err != nil {
		return SearchOutcome{}, err
	}
	if err := checkSearchSignals(page); err != nil {
		return SearchOutcome{}, err
	}

	if len(pending) > 0 {
		// 悬停在筛选按钮上展开面板
		filterButton, err := page.Element(`div.filter`)
		if err != nil {
			return SearchOutcome{}, fmt.Errorf("find filter button: %w", err)
		}
		if err := humanize.Hover(filterButton); err != nil {
			return SearchOutcome{}, fmt.Errorf("悬停筛选按钮失败: %w", err)
		}
		humanize.Delay(ctx, humanize.BeforeClick)
		if err := ctx.Err(); err != nil {
			return SearchOutcome{}, err
		}

		// 等待筛选面板出现
		if err := page.Wait(rod.Eval(`() => document.querySelector('div.filter-panel') !== null`)); err != nil {
			return SearchOutcome{}, fmt.Errorf("wait filter panel: %w", err)
		}

		// 记下筛选前的结果，用来判断筛选后的数据什么时候到位
		before := readFeedIDs(page)
		if _, err := page.Eval(`() => { if(window.__xhsSearchBatches)window.__xhsSearchBatches.length=0 }`); err != nil {
			return SearchOutcome{}, fmt.Errorf("reset captured search results before filters: %w", err)
		}

		// 指针先进面板，之后的移动都限制在面板内。
		// 从按钮直接拉到选项的话，路径会有一段既不在按钮上、也还没进面板。
		bounds, err := panelBounds(page)
		if err != nil {
			return SearchOutcome{}, err
		}
		if err := humanize.MoveInto(page, bounds); err != nil {
			return SearchOutcome{}, fmt.Errorf("移入筛选面板失败: %w", err)
		}

		// 用 ClickNoWait：筛选面板是 hover 浮层，rod 的 WaitInteractable 会误判被遮挡而死等。
		for _, pf := range pending {
			option, err := findFilterOption(page, pf)
			if err != nil {
				return SearchOutcome{}, err
			}
			// 每轮重取：点完一项后面板可能重排。
			if b, err := panelBounds(page); err == nil {
				bounds = b
			}
			humanize.Delay(ctx, humanize.BeforeClick)
			if err := ctx.Err(); err != nil {
				return SearchOutcome{}, err
			}
			if err := humanize.ClickNoWaitInside(option, bounds); err != nil {
				return SearchOutcome{}, fmt.Errorf("点击筛选选项「%s」失败: %w", pf.option, err)
			}
		}

		if err := waitFeedsChanged(ctx, page, before, 15*time.Second); err != nil {
			return SearchOutcome{}, err
		}
	}

	// 首批接受边界：先确认「真的采到了东西」，再交给采集器。判定阶段消费式读到的
	// 数据必须原样带过去（SPEC §4/§6），否则判定本身就吃掉了首批。
	src := &rodSearchSource{page: page, skipInitialState: len(pending) > 0}
	primed, err := waitFirstCollectable(ctx, src, searchFirstBatchBudget, searchFirstBatchPoll)
	if err != nil {
		return SearchOutcome{}, err
	}
	src.primed = primed
	return collectSearch(ctx, src, cfg)
}

// waitSearchReady 等搜索页达到「可以往下走」的状态。替代原先的 WaitStable。
//
// 为什么不能再等 WaitStable（rod v0.116.2，已核对源码）：
//   - WaitStable = utils.All(WaitLoad, WaitRequestIdle(d), WaitDOMStable(d))，All 是**并发**
//     启动三个等待并等它们**全部**结束——不是「任一满足即可」，也不是顺序等待；
//   - WaitRequestIdle 的返回类型是 func()，**它没有 error 可返回**：它的闭包要等
//     idleCounter 出现「连续 d 秒无计数在途请求」或 page 的 context 结束才返回。搜索页
//     持续打点（t2…/api/v2/collect）、APM 上报与 IM 轮询，常常整段预算内都没有这种静默
//     窗口，于是这个等待只能等到 page context 到期；
//   - 因此 WaitStable 在页面早就可用的情况下，也会静默吃光整个 90s 预算，然后在已过期的
//     context 上让紧随其后的检查瞬间失败，报出与真实原因无关的
//     「search page state wait failed: context deadline exceeded」；
//   - 另外 WaitStable 用同一个 sync.Once 保存 WaitLoad 与 WaitDOMStable 中最先结束者的
//     结果（nil 也会被锁定），所以「先成功的 load 掩盖后来 DOM 等待的错误」也是同一类
//     静默返回 nil 的路径。实测：成功时 WaitStable 约 8 秒，失败时整整 89 秒。
//
// 现在的等待是有界的、语义明确的：唯一硬条件是「页面上下文可读」，load 完成只是尽力而为。
// **这里不再用 DOM 卡片或 IS 结构当就绪凭据** —— 采集器读不到 DOM，就绪判定必须与采集
// 能力对齐，那部分判定在 waitFirstCollectable（见 artifacts/first-batch/SPEC.md）。
//
// 「仅本步预算用尽才降级」：本步用 context.WithTimeout 派生自己的预算，因此可以区分
// 「本步等超了」（可降级继续）和「父 context 结束 / JS 或页面出错」（必须原样返回）。
func waitSearchReady(ctx context.Context, page *rod.Page) error {
	stepCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	if err := page.Context(stepCtx).
		Wait(rod.Eval(`() => window.__INITIAL_STATE__ !== undefined || document.readyState === 'complete'`)); err != nil {
		return fmt.Errorf("stage=page_context: search page state wait failed: %w", err)
	}

	loadCtx, cancelLoad := context.WithTimeout(ctx, 10*time.Second)
	defer cancelLoad()
	if err := page.Context(loadCtx).
		Wait(rod.Eval(`() => document.readyState === 'complete'`)); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("stage=document_complete: search page state wait failed: %w", err)
		}
		if loadCtx.Err() == nil {
			// 不是本步等超，而是 JS / 页面本身出错：不能当成「慢」吞掉。
			return fmt.Errorf("stage=document_complete: search page state wait failed: %w", err)
		}
		logrus.Warnf("stage=document_complete: 搜索页 load 未在 10 秒内完成，继续判定首批: %v", err)
	}
	return nil
}

// ErrSearchResultUnconfirmed 表示首批采集边界既没有读到可采集的笔记，也没有可信证据
// 表明「当前查询确实为空」。调用方必须把它当作**未知**处理，绝不能当作「搜索确实没有结果」。
// 本期没有实现「可信的空完成证据」（依赖已取消的响应级契约），因此真实零结果也会走到这里，
// 这是相对「静默返回空数组」的有意收紧（见 artifacts/first-batch/SPEC.md §4）。
var ErrSearchResultUnconfirmed = errors.New("search_result_unconfirmed")

const (
	// searchFirstBatchBudget 是等「首批可采集数据」出现的预算。
	searchFirstBatchBudget = 8 * time.Second
	searchFirstBatchPoll   = 250 * time.Millisecond
)

// searchProbe 是一次不消费的探针结果，只用于判定与诊断，不含任何 token。
type searchProbe struct {
	Collectable     int
	HasInitialState bool
	Batches         int
	DOMNotes        int
}

func (p searchProbe) String() string {
	return fmt.Sprintf("initial_state=%t captured_batches=%d dom_notes=%d collectable=%d",
		p.HasInitialState, p.Batches, p.DOMNotes, p.Collectable)
}

// collectableSource 是首批接受边界需要的最小接口：能「只读不消费」地探一次，也能
// 消费式真读一次。
type collectableSource interface {
	probe() (searchProbe, error)
	read() ([]Feed, error)
}

// waitFirstCollectable 在预算内等「至少一条可采集笔记」出现，并把这份数据消费式读出来
// 交给调用方（调用方必须把它作为首批交给采集器，不能丢）。
//
// 判定口径（SPEC §4）：
//   - 探针读到 ≥1 条可采集 note → 消费式真读一次，读到的就是首批；
//   - 预算内始终读不到 → ErrSearchResultUnconfirmed（绝不返回成功的空结果）；
//   - 探针/真读报错、或父 context 取消/超时 → 原样返回该错误，不降级、不伪装。
func waitFirstCollectable(ctx context.Context, src collectableSource, budget, poll time.Duration) ([]Feed, error) {
	start := time.Now()
	deadline := start.Add(budget)
	var last searchProbe
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w: stage=first_collectable_batch budget=%s elapsed=%s %s",
				ErrSearchResultUnconfirmed, budget, time.Since(start).Round(time.Millisecond), last)
		}
		p, err := src.probe()
		if parentErr := ctx.Err(); parentErr != nil {
			return nil, parentErr
		}
		if err != nil {
			return nil, fmt.Errorf("stage=first_collectable_batch: probe search results: %w", err)
		}
		last = p
		if p.Collectable > 0 {
			feeds, err := src.read()
			if parentErr := ctx.Err(); parentErr != nil {
				return nil, parentErr
			}
			if err != nil {
				return nil, err
			}
			if len(feeds) > 0 {
				return feeds, nil
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sleep := min(poll, time.Until(deadline))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(sleep):
		}
	}
}

// feedIDsJS 读当前结果集的 id 列表，用来判断数据有没有换一批。
const feedIDsJS = `() => {
	const f = window.__INITIAL_STATE__?.search?.feeds;
	const v = f ? (f.value !== undefined ? f.value : f._value) : null;
	const ids=(v||[]).map(x=>x.id).filter(Boolean);
	for(const b of (window.__xhsSearchBatches||[])){for(const x of (b?.data?.items||[])){if(x.id)ids.push(x.id)}}
	return ids.join(",");
}`

func readFeedIDs(page *rod.Page) string {
	res, err := page.Eval(feedIDsJS)
	if err != nil {
		return ""
	}
	return res.Value.Str()
}

// waitFeedsChanged 等筛选后的数据到位。
//
// 点完筛选项之后不能立刻读结果：站点是先把 feeds 清空、再灌入新数据，
// 中间这段时间读到的要么是空，要么还是筛选前那一批。原先用
// MustWait(__INITIAL_STATE__ !== undefined) 等，而这个条件从首屏起就为真、
// 立即返回，等于没等——多个筛选项一起用时表现为只有一部分生效。
//
// 超时不报错：筛选已经点上了，宁可返回可能偏旧的数据，也不要整个搜索失败。
func waitFeedsChanged(ctx context.Context, page *rod.Page, before string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if now := readFeedIDs(page); now != "" && now != before {
			return nil
		}
		if err := waitSearchDelay(ctx, 300*time.Millisecond); err != nil {
			return err
		}
	}
	logrus.Warnf("筛选后等待结果刷新超时（%s）", timeout)
	return fmt.Errorf("filtered search results did not refresh within %s", timeout)
}

// panelBounds 取筛选面板当前占据的矩形。
func panelBounds(page *rod.Page) (humanize.Rect, error) {
	panel, err := page.Element("div.filter-panel")
	if err != nil {
		return humanize.Rect{}, fmt.Errorf("读取筛选面板失败: %w", err)
	}
	shape, err := panel.Shape()
	if err != nil {
		return humanize.Rect{}, fmt.Errorf("读取筛选面板失败: %w", err)
	}
	if len(shape.Quads) == 0 {
		return humanize.Rect{}, fmt.Errorf("筛选面板没有可用区域")
	}
	q := shape.Quads[0]
	return humanize.Rect{Left: q[0], Top: q[1], Right: q[4], Bottom: q[5]}, nil
}

// findFilterOption 在筛选面板里定位一个选项：按标签找到组，再在组内按文本找选项。
//
// 全程不用序号。同一个选项在面板里可能渲染成多个 div.tags（数量随视口而变，
// 且首项是否重复各组不一致），下标对不齐；早前用 div.tags:nth-child(N) 会选错项。
// 同一个文本可能对应多个节点，只取其中可见的那个。
//
// 作用域必须限定在 div.filter-panel 内且只认 div.tags：页面别处存在同文本的
// 可见元素（顶部频道栏的「图文」「视频」、标签「综合」），放宽会点错地方。
func findFilterOption(page *rod.Page, pf pendingFilter) (*rod.Element, error) {
	groups, err := page.Elements("div.filter-panel div.filters")
	if err != nil {
		return nil, fmt.Errorf("读取筛选面板失败: %w", err)
	}

	for _, group := range groups {
		// 组标签是 div.filters 下的直接子 span
		label, err := group.Element(":scope > span")
		if err != nil {
			continue
		}
		text, err := label.Text()
		if err != nil || strings.TrimSpace(text) != pf.group {
			continue
		}

		options, err := group.Elements("div.tags")
		if err != nil {
			return nil, fmt.Errorf("读取「%s」的选项失败: %w", pf.group, err)
		}

		var available []string
		matched := false
		for _, opt := range options {
			t, err := opt.Text()
			if err != nil {
				continue
			}
			t = strings.TrimSpace(t)
			available = append(available, t)
			if t != pf.option {
				continue
			}

			matched = true
			if humanize.Visible(opt) {
				return opt, nil
			}
		}
		if matched {
			return nil, fmt.Errorf("「%s」里的选项「%s」当前不可见", pf.group, pf.option)
		}
		return nil, fmt.Errorf("「%s」里没有选项「%s」，页面上是：%s",
			pf.group, pf.option, strings.Join(available, "、"))
	}

	return nil, fmt.Errorf("筛选面板里没有「%s」这一组", pf.group)
}

func makeSearchURL(keyword string) string {

	values := url.Values{}
	values.Set("keyword", keyword)
	values.Set("source", "web_explore_feed")

	//https://www.xiaohongshu.com/search_result?keyword=%25E7%258E%258B%25E5%25AD%2590&source=web_search_result_notes
	//https://www.xiaohongshu.com/search_result?keyword=%25E7%258E%258B%25E5%25AD%2590&source=web_explore_feed
	return fmt.Sprintf("https://www.xiaohongshu.com/search_result?%s", values.Encode())
}
