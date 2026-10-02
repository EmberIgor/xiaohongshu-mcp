package xiaohongshu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/stretchr/testify/require"
)

// Opt-in offline browser fixture. XHS_TEST_BROWSER must name a standalone Chromium binary.
func TestSearchBrowserFixture(t *testing.T) {
	bin := os.Getenv("XHS_TEST_BROWSER")
	if bin == "" {
		t.Skip("set XHS_TEST_BROWSER to a standalone Chromium path")
	}
	fixtureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><div id='keep'>fixture</div></body></html>"))
	}))
	defer fixtureServer.Close()
	control, err := launcher.New().Bin(bin).Headless(true).NoSandbox(true).Launch()
	require.NoError(t, err)
	b := rod.New().ControlURL(control).MustConnect()
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = b.Close()
		}
	})
	p := b.MustPage(fixtureServer.URL + "/fixture")
	pageClosed := false
	t.Cleanup(func() {
		if !pageClosed {
			_ = p.Close()
		}
	})
	_, err = p.EvalOnNewDocument(searchCaptureHook)
	require.NoError(t, err)
	_, err = p.Eval(`() => {
 window.__INITIAL_STATE__={search:{feeds:{value:[{id:'old',modelType:'note',xsecToken:'preserve',noteCard:{displayTitle:'stale'}}]}}};
		const payload=window.fixturePayload={data:{items:[{id:'new',model_type:'note',xsec_token:'token-new',note_card:{display_title:'new title',user:{user_id:'u',nickname:'N'},cover:{url_default:'image'}}}]}};
 window.fetch=(...args)=>Promise.resolve({clone:()=>({text:()=>Promise.resolve(JSON.stringify(payload))})});
 class FakeXHR {open(m,u){this.url=u} addEventListener(n,fn){this.fn=fn} send(){this.responseType='json';this.response=payload;this.fn()}}
 window.XMLHttpRequest=FakeXHR;
 document.body.insertAdjacentHTML('beforeend','<div class="captcha-hidden" style="display:none;width:0;height:0"></div><div class="login-modal" style="display:none;width:0;height:0"></div>');
	}`)
	require.NoError(t, err)
	// Reinstall after the fixture replaces fetch/XHR so hooks observe both response paths.
	_, err = p.Eval(`() => {` + searchCaptureHook + `}`)
	require.NoError(t, err)
	_, err = p.Eval(`() => { window.fetch('/api/sns/web/v1/search/notes'); const x=new XMLHttpRequest();x.open('GET','/api/sns/web/v1/search/notes');x.send(); }`)
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond)
	src := &rodSearchSource{page: p}
	rawFeeds, err := src.read()
	require.NoError(t, err)
	require.Len(t, rawFeeds, 2, "captured responses should supersede stale initial state")
	require.Equal(t, "new", rawFeeds[0].ID)
	acc := searchAccumulator{}
	acc.add(rawFeeds)
	feeds := acc.feeds()
	require.Len(t, feeds, 1)
	require.Equal(t, "token-new", feeds[0].XsecToken)
	require.Equal(t, "new title", feeds[0].NoteCard.DisplayTitle)
	require.Equal(t, "u", feeds[0].NoteCard.User.UserID)
	require.Equal(t, "image", feeds[0].NoteCard.Cover.URLDefault)
	require.NoError(t, checkSearchSignals(p))
	_, err = p.Eval(`() => { const e=document.createElement('div');e.className='captcha-visible';e.style='width:20px;height:20px';document.body.append(e) }`)
	require.NoError(t, err)
	require.ErrorContains(t, checkSearchSignals(p), "risk_control")
	_, err = p.Eval(`() => { document.querySelector('.captcha-visible').remove();const e=document.createElement('div');e.className='login-modal';e.style='width:20px;height:20px';document.body.append(e) }`)
	require.NoError(t, err)
	require.ErrorContains(t, checkSearchSignals(p), "login_required")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, waitFeedsChanged(ctx, p, "", time.Second), context.Canceled)
	// Also verify the browser-visible queue is consumptive.
	var queue []json.RawMessage
	res, e := p.Eval(`() => JSON.stringify(window.__xhsSearchBatches||[])`)
	require.NoError(t, e)
	require.NoError(t, json.Unmarshal([]byte(res.Value.Str()), &queue))
	require.Empty(t, queue)
	// A consumed queue must still be the array retained by the hook closure.
	_, err = p.Eval(`() => { window.fixturePayload.data.items[0].xsec_token='token-duplicate'; window.fetch('/api/sns/web/v1/search/notes'); }`)
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond)
	later, err := src.read()
	require.NoError(t, err)
	require.Len(t, later, 1, "a stale state snapshot must not reappear after its first read")
	require.Equal(t, "new", later[0].ID)
	require.Zero(t, acc.add(later), "duplicate id must not replace the first note and its xsec_token")
	require.Equal(t, "token-new", acc.feeds()[0].XsecToken)
	_, err = p.Eval(`() => { window.fixturePayload.data.items[0].id='later'; window.fixturePayload.data.items[0].xsec_token='token-later'; window.fetch('/api/sns/web/v1/search/notes'); }`)
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond)
	unique, err := src.read()
	require.NoError(t, err)
	require.Len(t, unique, 1, "capture queue must remain live after duplicate batches")
	require.Equal(t, "later", unique[0].ID)
	require.Equal(t, 1, acc.add(unique))
	require.Len(t, acc.feeds(), 2)
	_, err = p.Eval(`() => { window.__xhsSearchBatches.length=0; window.fixturePayload.data.items[0].id='filtered'; window.fixturePayload.data.items[0].xsec_token='token-filtered'; window.__INITIAL_STATE__.search.feeds.value=[{id:'old-filter',modelType:'note',xsecToken:'old-token',noteCard:{displayTitle:'stale filtered result'}}] }`)
	require.NoError(t, err)
	filteredSource := &rodSearchSource{page: p, skipInitialState: true}
	filtered, err := filteredSource.read()
	require.NoError(t, err)
	require.Empty(t, filtered, "post-filter reads must exclude stale initial state")
	_, err = p.Eval(`() => window.fetch('/api/sns/web/v1/search/notes')`)
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond)
	filtered, err = filteredSource.read()
	require.NoError(t, err)
	require.Len(t, filtered, 1, "capture hook must remain live after filtered queue clear")
	require.Equal(t, "filtered", filtered[0].ID)
	require.NoError(t, p.Close())
	pageClosed = true
	require.NoError(t, b.Close())
	closed = true
}
