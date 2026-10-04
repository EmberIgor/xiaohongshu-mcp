package xiaohongshu

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/stretchr/testify/require"
)

// Offline browser coverage is opt-in; the only navigated origin is this loopback fixture.
func TestProfileBrowserFixtures(t *testing.T) {
	bin := os.Getenv("XHS_TEST_BROWSER")
	if bin == "" {
		t.Skip("set XHS_TEST_BROWSER to a standalone Chromium path")
	}
	profiles := map[string]string{
		"valid-delayed":      `setTimeout(()=>window.__INITIAL_STATE__={user:{userId:"0123456789abcdef01234567",userPageData:{value:{basicInfo:{userId:"0123456789abcdef01234567",nickname:"Fixture"},interactions:[]}},notes:{value:[[]]},activeTab:{value:{index:0,query:"note"}}}},120)`,
		"noteCard-author":    `window.__INITIAL_STATE__={user:{userPageData:{value:{basicInfo:{nickname:"Fixture"},interactions:[]}},notes:{_value:[[{id:"own-1",modelType:"note",noteCard:{type:"normal",displayTitle:"Own note",user:{userId:{_value:"0123456789abcdef01234567"},nickname:"Fixture"}}}]]},activeTab:{_value:{index:0,query:"note"}}}}`,
		"author-conflict":    `window.__INITIAL_STATE__={user:{userPageData:{value:{basicInfo:{nickname:"Fixture"},interactions:[]}},notes:{value:[[{modelType:"note",noteCard:{user:{userId:"0123456789abcdef01234567"}}},{modelType:"note",noteCard:{user:{userId:"fedcba9876543210fedcba98"}}}]]},activeTab:{value:{index:0,query:"note"}}}}`,
		"mismatch":           `window.__INITIAL_STATE__={user:{userId:"fedcba9876543210fedcba98",userPageData:{value:{basicInfo:{userId:"fedcba9876543210fedcba98",nickname:"Fixture"},interactions:[]}},notes:{value:[[]]},activeTab:{value:{index:0,query:"note"}}}}`,
		"state-conflict":     `window.__INITIAL_STATE__={user:{userId:"fedcba9876543210fedcba98",userPageData:{value:{basicInfo:{userId:"0123456789abcdef01234567",nickname:"Fixture"},interactions:[]}},notes:{value:[[]]},activeTab:{value:{index:0,query:"note"}}}}`,
		"author-mismatch":    `window.__INITIAL_STATE__={user:{userId:"0123456789abcdef01234567",userPageData:{value:{basicInfo:{userId:"0123456789abcdef01234567",nickname:"Fixture"},interactions:[]}},notes:{value:[[{user:{user_id:"fedcba9876543210fedcba98"}}]]},activeTab:{value:{index:0,query:"note"}}}}`,
		"other-author-fav":   `window.__INITIAL_STATE__={user:{userId:"0123456789abcdef01234567",userPageData:{value:{basicInfo:{userId:"0123456789abcdef01234567",nickname:"Fixture"},interactions:[]}},notes:{value:[[{user:{user_id:"fedcba9876543210fedcba98"}}]]},activeTab:{value:{index:0,query:"fav"}}}}`,
		"other-author-liked": `window.__INITIAL_STATE__={user:{userId:"0123456789abcdef01234567",userPageData:{value:{basicInfo:{userId:"0123456789abcdef01234567",nickname:"Fixture"},interactions:[]}},notes:{value:[[{user:{user_id:"fedcba9876543210fedcba98"}}]]},activeTab:{value:{index:0,query:"liked"}}}}`,
		"empty":              `window.__INITIAL_STATE__={user:{userPageData:{value:{}},notes:{value:[[]]}}}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind := r.URL.Query().Get("fixture")
		body := ""
		switch kind {
		case "login":
			body = "立即登录"
		case "captcha":
			body = "验证码"
		case "risk":
			body = "访问异常 操作频繁 风险控制"
		case "unavailable":
			body = "暂时无法访问"
		default:
			body = "profile fixture"
		}
		fmt.Fprintf(w, `<html><body>%s<script>%s</script></body></html>`, body, profiles[kind])
	}))
	defer server.Close()
	control, err := launcher.New().Bin(bin).Headless(true).NoSandbox(true).Launch()
	require.NoError(t, err)
	b := rod.New().ControlURL(control).MustConnect()
	defer func() { require.NoError(t, b.Close()) }()
	for _, tc := range []struct {
		name, expect string
		ok           bool
	}{
		{"valid-delayed", "", true}, {"noteCard-author", "", true}, {"author-conflict", "note author identity mismatch", false}, {"mismatch", "structured account UID", false}, {"state-conflict", "identity fields conflict", false}, {"author-mismatch", "note author identity mismatch", false}, {"other-author-fav", "", true}, {"other-author-liked", "", true}, {"empty", "account UID", false},
		{"login", "requires login", false}, {"captcha", "risk control", false}, {"risk", "risk control", false}, {"unavailable", "profile page unavailable", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := b.MustPage("about:blank")
			t.Cleanup(func() { require.NoError(t, p.Close()) })
			a := NewUserProfileAction(p)
			a.profileURL = func(_, _ string, _ ProfileTab) string {
				return server.URL + "/user/profile/0123456789abcdef01234567?fixture=" + tc.name
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tab := TabNotes
			if tc.name == "other-author-fav" {
				tab = TabFavorites
			}
			if tc.name == "other-author-liked" {
				tab = TabLiked
			}
			got, err := a.UserProfile(ctx, "0123456789abcdef01234567", "", tab)
			if tc.ok {
				require.NoError(t, err)
				require.Equal(t, "0123456789abcdef01234567", got.UserID)
				require.Equal(t, "Fixture", got.UserBasicInfo.Nickname)
			} else {
				require.Error(t, err)
				require.ErrorContains(t, err, tc.expect)
			}
		})
	}
	t.Run("get-my-profile-delayed-hydration", func(t *testing.T) {
		p := b.MustPage("about:blank")
		defer func() { require.NoError(t, p.Close()) }()
		_, err := p.Eval(`() => setTimeout(() => window.__INITIAL_STATE__ = {}, 100)`)
		require.NoError(t, err)
		start := time.Now()
		require.NoError(t, waitForHydratedProfileState(p))
		require.GreaterOrEqual(t, time.Since(start), 75*time.Millisecond)
	})
	t.Run("get-my-profile-allows-legacy-state-without-uid", func(t *testing.T) {
		p := b.MustPage("about:blank")
		defer func() { require.NoError(t, p.Close()) }()
		_, err := p.Eval(`() => { window.__INITIAL_STATE__ = {user:{userPageData:{value:{basicInfo:{nickname:"Legacy"},interactions:[]}},notes:{value:[[]]},activeTab:{value:{index:0,query:"note"}}}} }`)
		require.NoError(t, err)
		a := NewUserProfileAction(p)
		got, err := a.extractUserProfileData(p, TabNotes, false)
		require.NoError(t, err)
		require.Empty(t, got.UserID)
		require.Equal(t, "Legacy", got.UserBasicInfo.Nickname)
		_, err = a.extractUserProfileData(p, TabNotes, true)
		require.ErrorContains(t, err, "structured account UID unavailable")
	})
	t.Run("cancel-state-wait", func(t *testing.T) {
		p := b.MustPage("about:blank")
		defer func() { require.NoError(t, p.Close()) }()
		a := NewUserProfileAction(p)
		a.profileURL = func(_, _ string, _ ProfileTab) string {
			return server.URL + "/user/profile/0123456789abcdef01234567?fixture=unavailable"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		_, err := a.UserProfile(ctx, "0123456789abcdef01234567", "", TabNotes)
		require.Error(t, err)
	})
}
