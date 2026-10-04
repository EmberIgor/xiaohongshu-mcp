package xiaohongshu

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
)

// ProfileTab 个人主页的子 tab。
type ProfileTab string

const (
	TabNotes     ProfileTab = "note"
	TabFavorites ProfileTab = "fav"
	TabLiked     ProfileTab = "liked"
)

// ParseProfileTab 解析 tab 名，空值默认为「笔记」。
func ParseProfileTab(s string) (ProfileTab, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "", "note", "notes", "笔记":
		return TabNotes, nil
	case "fav", "favorites", "favorite", "收藏":
		return TabFavorites, nil
	case "liked", "like", "点赞":
		return TabLiked, nil
	}
	return "", fmt.Errorf("未知的主页 tab %q，可选：note / fav / liked", s)
}

// tabLabel 子 tab 对应的页面文字。
var tabLabel = map[ProfileTab]string{
	TabNotes:     "笔记",
	TabFavorites: "收藏",
	TabLiked:     "点赞",
}

type UserProfileAction struct {
	page       *rod.Page
	profileURL func(string, string, ProfileTab) string
}

func NewUserProfileAction(page *rod.Page) *UserProfileAction {
	pp := page.Timeout(60 * time.Second)
	return &UserProfileAction{page: pp, profileURL: makeUserProfileURL}
}

// UserProfile 获取用户基本信息及指定 tab 下的帖子
func (u *UserProfileAction) UserProfile(ctx context.Context, userID, xsecToken string, tab ProfileTab) (*UserProfileResponse, error) {
	page := u.page.Context(ctx).Timeout(60 * time.Second) // 重设被 .Context 清掉的 deadline

	buildURL := u.profileURL
	if buildURL == nil {
		buildURL = makeUserProfileURL
	}
	searchURL := buildURL(userID, xsecToken, tab)
	if err := page.Navigate(searchURL); err != nil {
		return nil, fmt.Errorf("profile navigation failed: %w", err)
	}
	if err := page.WaitStable(2 * time.Second); err != nil {
		return nil, fmt.Errorf("profile page did not stabilize: %w", err)
	}
	if err := page.Wait(rod.Eval(`() => window.__INITIAL_STATE__ !== undefined || /登录|验证码|安全验证|访问异常|操作频繁|风险控制|暂时无法访问|页面不存在|无法访问/.test(document.body?.innerText || '')`)); err != nil {
		return nil, fmt.Errorf("profile state unavailable: %w", err)
	}
	state, err := page.Eval(`() => JSON.stringify({url: location.pathname, text: (document.body?.innerText || '').slice(0, 12000)})`)
	if err != nil {
		return nil, fmt.Errorf("profile page state unavailable: %w", err)
	}
	var pageState struct {
		URL  string `json:"url"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(state.Value.Str()), &pageState); err != nil {
		return nil, fmt.Errorf("invalid profile page state")
	}
	if strings.Contains(pageState.Text, "验证码") || strings.Contains(pageState.Text, "安全验证") || strings.Contains(pageState.Text, "访问异常") || strings.Contains(pageState.Text, "操作频繁") || strings.Contains(pageState.Text, "风险控制") {
		return nil, fmt.Errorf("profile access stopped by platform risk control")
	}
	if strings.Contains(pageState.Text, "登录后查看更多") || strings.Contains(pageState.Text, "立即登录") || strings.Contains(pageState.Text, "扫码登录") {
		return nil, fmt.Errorf("profile requires login")
	}
	if strings.Contains(pageState.Text, "暂时无法访问") || strings.Contains(pageState.Text, "页面不存在") || strings.Contains(pageState.Text, "无法访问") {
		return nil, fmt.Errorf("profile page unavailable")
	}
	if pageState.URL != "/user/profile/"+userID {
		return nil, fmt.Errorf("profile identity mismatch: requested account page was not loaded")
	}

	result, err := u.extractUserProfileData(page, tab, true)
	if err != nil {
		return nil, err
	}
	if result.UserID != userID {
		return nil, fmt.Errorf("profile identity mismatch: structured account UID did not match request")
	}
	return result, nil
}

// extractUserProfileData 从页面中提取用户资料数据的通用方法
func (u *UserProfileAction) extractUserProfileData(page *rod.Page, tab ProfileTab, requireIdentity bool) (*UserProfileResponse, error) {
	userEval, err := page.Eval(`() => {
		if (window.__INITIAL_STATE__ &&
		    window.__INITIAL_STATE__.user &&
		    window.__INITIAL_STATE__.user.userPageData) {
			const userPageData = window.__INITIAL_STATE__.user.userPageData;
			let data = userPageData;
			for (let i = 0; i < 5 && data && typeof data === 'object'; i++) {
			  if (data.value !== undefined) data = data.value;
			  else if (data._value !== undefined) data = data._value;
			  else break;
			}
			if (data) {
				const unwrap = (v) => {
				  for (let i = 0; i < 5 && v && typeof v === 'object'; i++) {
				    if (v.value !== undefined) v = v.value;
				    else if (v._value !== undefined) v = v._value;
				    else break;
				  }
				  return v;
				};
				const basic = unwrap(data.basicInfo) || {};
				basic.userId = unwrap(basic.userId);
				basic.user_id = unwrap(basic.user_id);
				data.basicInfo = basic;
				return JSON.stringify(data);
			}
		}
		return "";
	}`)
	if err != nil {
		return nil, fmt.Errorf("profile structured state unavailable: %w", err)
	}
	userDataResult := userEval.Value.Str()

	if userDataResult == "" {
		return nil, fmt.Errorf("user.userPageData.value not found in __INITIAL_STATE__")
	}

	// 2. 获取用户帖子及当前 tab：window.__INITIAL_STATE__.user
	notesEval, err := page.Eval(`() => {
		const u = window.__INITIAL_STATE__ && window.__INITIAL_STATE__.user;
		if (!u || !u.notes) return "";
		const unwrap = (o) => {
		  let v = o;
		  for (let i = 0; i < 5 && v && typeof v === 'object'; i++) {
		    if (v.value !== undefined) v = v.value;
		    else if (v._value !== undefined) v = v._value;
		    else break;
		  }
		  return v;
		};
		const notes = unwrap(u.notes);
		if (!notes) return "";
		const active = unwrap(u.activeTab) || {};
		const query = active.query || "";
		const uid = (v) => { v = unwrap(v); return typeof v === 'string' ? v.trim() : ''; };
		const authors = [];
		if (!query || query === "note") for (const group of (Array.isArray(notes) ? notes : [])) for (const n of (Array.isArray(group) ? group : [])) {
		  const collect = (user) => {
		    if (!user) return;
		    for (const field of ['userId', 'user_id']) {
		      const id = uid(user[field]);
		      if (id) authors.push(id);
		    }
		  };
		  collect(n?.noteCard?.user);
		  collect(n?.noteCard?.userInfo);
		  collect(n?.user);
		  collect(n?.userInfo);
		  if (n?.noteCard?.user) n.noteCard.user.userId = uid(n.noteCard.user.userId) || uid(n.noteCard.user.user_id);
		}
		return JSON.stringify({notes: notes, index: active.index || 0, query: query, userId: uid(u.userId) || uid(u.user_id) || uid(u.uid), authors});
	}`)
	if err != nil {
		return nil, fmt.Errorf("profile notes state unavailable: %w", err)
	}
	notesResult := notesEval.Value.Str()

	if notesResult == "" {
		return nil, fmt.Errorf("user.notes.value not found in __INITIAL_STATE__")
	}

	// 解析用户信息
	var userPageData struct {
		Interactions []UserInteractions `json:"interactions"`
		BasicInfo    struct {
			UserBasicInfo
			UserID    string `json:"userId"`
			UserIDAlt string `json:"user_id"`
		} `json:"basicInfo"`
	}
	if err := json.Unmarshal([]byte(userDataResult), &userPageData); err != nil {
		return nil, fmt.Errorf("failed to unmarshal userPageData: %w", err)
	}

	var notesData struct {
		Notes   [][]Feed `json:"notes"`
		Index   int      `json:"index"`
		Query   string   `json:"query"`
		UserID  string   `json:"userId"`
		Authors []string `json:"authors"`
	}
	if err := json.Unmarshal([]byte(notesResult), &notesData); err != nil {
		return nil, fmt.Errorf("failed to unmarshal notes: %w", err)
	}
	identity := userPageData.BasicInfo.UserID
	if identity == "" {
		identity = userPageData.BasicInfo.UserIDAlt
	}
	if identity != "" && notesData.UserID != "" && identity != notesData.UserID {
		return nil, fmt.Errorf("profile structured identity fields conflict")
	}
	if identity == "" {
		identity = notesData.UserID
	}
	if identity != "" && !validProfileID(identity) {
		return nil, fmt.Errorf("profile structured account UID unavailable")
	}
	if tab == "" || tab == TabNotes {
		for _, author := range notesData.Authors {
			if !validProfileID(author) {
				return nil, fmt.Errorf("profile note author identity mismatch")
			}
			if identity != "" && author != identity {
				return nil, fmt.Errorf("profile note author identity mismatch")
			}
			if identity == "" {
				identity = author
			}
		}
	}
	if identity == "" && requireIdentity {
		return nil, fmt.Errorf("profile structured account UID unavailable")
	}
	// tab 不符时报错，避免把别的 tab 的内容当成结果返回
	want := tab
	if want == "" {
		want = TabNotes
	}
	if notesData.Query != "" && ProfileTab(notesData.Query) != want {
		return nil, fmt.Errorf("当前 tab 为 %q，与请求的 %q 不符", notesData.Query, want)
	}

	// 组装响应
	response := &UserProfileResponse{
		UserID:        identity,
		UserBasicInfo: userPageData.BasicInfo.UserBasicInfo,
		Interactions:  userPageData.Interactions,
	}

	// 每个 tab 的内容存在各自的下标里，只取当前 tab 的，避免混入其他 tab
	if notesData.Index >= 0 && notesData.Index < len(notesData.Notes) {
		response.Feeds = append(response.Feeds, notesData.Notes[notesData.Index]...)
	}

	return response, nil
}

func waitForHydratedProfileState(page *rod.Page) error {
	return page.Wait(rod.Eval(`() => window.__INITIAL_STATE__ !== undefined`))
}

func makeUserProfileURL(userID, xsecToken string, tab ProfileTab) string {
	profileURL := fmt.Sprintf("https://www.xiaohongshu.com/user/profile/%s", userID)
	if xsecToken != "" {
		profileURL += "?xsec_token=" + url.QueryEscape(xsecToken) + "&xsec_source=pc_note"
	}
	if tab != "" && tab != TabNotes {
		separator := "?"
		if strings.Contains(profileURL, "?") {
			separator = "&"
		}
		profileURL += fmt.Sprintf("%stab=%s&subTab=note", separator, tab)
	}
	return profileURL
}

func (u *UserProfileAction) GetMyProfileViaSidebar(ctx context.Context, tab ProfileTab) (*UserProfileResponse, error) {
	page := u.page.Context(ctx).Timeout(60 * time.Second) // 重设被 .Context 清掉的 deadline

	// 创建导航动作
	navigate := NewNavigate(page)

	// 通过侧边栏导航到个人主页
	if err := navigate.ToProfilePage(ctx); err != nil {
		return nil, fmt.Errorf("failed to navigate to profile page via sidebar: %w", err)
	}

	// 等待页面加载完成并获取 __INITIAL_STATE__
	if err := page.WaitStable(2 * time.Second); err != nil {
		return nil, fmt.Errorf("profile page did not stabilize: %w", err)
	}
	if err := waitForHydratedProfileState(page); err != nil {
		return nil, fmt.Errorf("profile state unavailable: %w", err)
	}

	if err := u.selectTab(ctx, page, tab); err != nil {
		return nil, err
	}

	return u.extractUserProfileData(page, tab, false)
}

// selectTab 切到目标子 tab。「笔记」是默认 tab，无需点击。
func (u *UserProfileAction) selectTab(ctx context.Context, page *rod.Page, tab ProfileTab) error {
	if tab == "" || tab == TabNotes {
		return nil
	}

	label := tabLabel[tab]
	elems, err := page.Elements(`.reds-tab-item.sub-tab-list`)
	if err != nil {
		return fmt.Errorf("未找到主页子 tab: %w", err)
	}

	for _, elem := range elems {
		text, err := elem.Text()
		if err != nil || strings.TrimSpace(text) != label {
			continue
		}
		humanize.Delay(ctx, humanize.BeforeClick)
		if err := humanize.Click(elem); err != nil {
			return fmt.Errorf("切换到 %s 失败: %w", label, err)
		}
		humanize.Delay(ctx, humanize.AfterClick)
		page.MustWaitStable()
		return nil
	}
	return fmt.Errorf("未找到子 tab %q", label)
}
