package xiaohongshu

import (
	"context"
	"encoding/json"
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
func (r *rodSearchSource) read() ([]Feed, error) {
	v, e := r.page.Eval(`() => { const s=window.__INITIAL_STATE__; let f=s?.search?.feeds; if(f&&f.value!==undefined)f=f.value;else if(f&&f._value!==undefined)f=f._value; const batches=window.__xhsSearchBatches||[]; const captured=batches.splice(0); return JSON.stringify({state:Array.isArray(f)?f:[],batches:captured}); }`)
	if e != nil {
		return nil, fmt.Errorf("read search feed state: %w", e)
	}
	var snap struct {
		State   json.RawMessage   `json:"state"`
		Batches []json.RawMessage `json:"batches"`
	}
	if e = json.Unmarshal([]byte(v.Value.Str()), &snap); e != nil {
		return nil, e
	}
	var out []Feed
	decode := func(raw []byte) ([]Feed, error) {
		if len(raw) > 0 && raw[0] == '{' {
			return normalizeSearchItems(raw)
		}
		var fs []Feed
		er := json.Unmarshal(raw, &fs)
		return onlyNotes(fs), er
	}
	if !r.stateRead && !r.skipInitialState && len(snap.Batches) == 0 && len(snap.State) > 0 {
		fs, er := decode(snap.State)
		if er != nil {
			return nil, er
		}
		out = append(out, fs...)
	}
	r.stateRead = true
	for _, raw := range snap.Batches {
		fs, er := normalizeSearchItems(raw)
		if er != nil {
			return nil, er
		}
		out = append(out, fs...)
	}
	return out, nil
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
	return collectSearch(ctx, &rodSearchSource{page: page, skipInitialState: len(pending) > 0}, cfg)
}

// searchFirstScreenJS 判断首屏结果是否已经出来（IS 里的 feeds、被动捕获的
// 批次、或已经渲染出的笔记卡片，任一即可）。
const searchFirstScreenJS = `() => {
	const f = window.__INITIAL_STATE__?.search?.feeds;
	const v = f ? (f.value !== undefined ? f.value : f._value) : null;
	const hasState = Array.isArray(v) && v.length > 0;
	const hasBatch = (window.__xhsSearchBatches || []).some(b => (b?.data?.items || []).length > 0);
	const hasDom = document.querySelectorAll('#noteContainer .note-item, section.note-item').length > 0;
	return hasState || hasBatch || hasDom;
}`

// waitSearchReady 等搜索页就绪。替代原先的 WaitStable。
//
// 为什么不能再用 WaitStable：它内部的 WaitRequestIdle(2s) 要求「连续 2 秒没有
// 任何在途请求」，而搜索页会持续发打点（t2…/api/v2/collect）、APM 上报和 IM 轮询，
// 经常整段预算里都不出现这种静默窗口。更糟的是 WaitRequestIdle 的错误在 rod 内部
// 被丢弃、返回 nil，于是 WaitStable 会「成功地」静默吃掉整个 ctx 预算，让紧随其后的
// 等待在已过期的 context 上立刻失败，报出与真实原因无关的
// 「search page state wait failed: context deadline exceeded」。
// 实测：成功时 WaitStable 约 8 秒（等到静默窗口），失败时整整 89 秒。
//
// 改成三段各自有界、语义明确的等待：唯一硬条件是 __INITIAL_STATE__（采集真正依赖
// 的东西，通常 1~2 秒就位）；load 完成与首屏结果都是尽力而为，超时只记日志继续。
// 任何一步都不会吃光总预算，失败时的错误也指向真实原因。
func waitSearchReady(ctx context.Context, page *rod.Page) error {
	if err := page.Context(ctx).Timeout(25 * time.Second).
		Wait(rod.Eval(`() => window.__INITIAL_STATE__ !== undefined`)); err != nil {
		return fmt.Errorf("search page state wait failed: %w", err)
	}
	if err := page.Context(ctx).Timeout(10 * time.Second).
		Wait(rod.Eval(`() => document.readyState === 'complete'`)); err != nil {
		logrus.Warnf("搜索页 load 未在 10 秒内完成，继续采集: %v", err)
	}
	if err := page.Context(ctx).Timeout(8 * time.Second).
		Wait(rod.Eval(searchFirstScreenJS)); err != nil {
		logrus.Warnf("首屏结果未在 8 秒内就绪，继续采集: %v", err)
	}
	return nil
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
