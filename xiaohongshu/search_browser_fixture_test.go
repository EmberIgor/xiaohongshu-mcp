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

	// —— 首批接受边界（artifacts/first-batch/SPEC.md §8 的 8/9/10）——
	// 8) 探针必须只读不消费：连续两次 probe 之后，捕获队列还得原封不动。
	_, err = p.Eval(`() => { window.__xhsSearchBatches.length=0; window.__INITIAL_STATE__={search:{feeds:{value:[]}}}; window.fixturePayload.data.items[0].id='first'; window.fixturePayload.data.items[0].xsec_token='token-first' }`)
	require.NoError(t, err)
	_, err = p.Eval(`() => window.fetch('/api/sns/web/v1/search/notes')`)
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond)
	probeSource := &rodSearchSource{page: p}
	first, err := probeSource.probe()
	require.NoError(t, err)
	require.Equal(t, 1, first.Collectable)
	require.True(t, first.HasInitialState)
	require.Equal(t, 1, evalJSONInt(t, p, `() => (window.__xhsSearchBatches||[]).length`), "probe must not consume the capture queue")

	// 9) 判定阶段的真读，必须作为首批进入采集器，且只交付一次。
	primed, err := waitFirstCollectable(context.Background(), probeSource, time.Second, 5*time.Millisecond)
	require.NoError(t, err)
	require.Len(t, primed, 1)
	require.Equal(t, "first", primed[0].ID)
	require.Equal(t, 0, evalJSONInt(t, p, `() => (window.__xhsSearchBatches||[]).length`), "the accepted read consumes the queue")
	probeSource.primed = primed
	primedAcc := searchAccumulator{}
	batch, err := probeSource.read()
	require.NoError(t, err)
	require.Len(t, batch, 1, "primed first batch must reach the collector")
	require.Equal(t, 1, primedAcc.add(batch))
	require.Equal(t, "token-first", primedAcc.feeds()[0].XsecToken)
	again, err := probeSource.read()
	require.NoError(t, err)
	require.Empty(t, again, "primed first batch must not be delivered twice")

	// 10) B1 反例：IS 存在但 feeds 为空、队列为空、DOM 有笔记卡片 —— 修复前这里会
	// 走「成功的 0 条」，现在必须是可识别的未确认。
	_, err = p.Eval(`() => {
		window.__xhsSearchBatches.length=0;
		window.__INITIAL_STATE__={search:{feeds:{value:[]}}};
		document.body.insertAdjacentHTML('beforeend','<section class="note-item"></section>');
	}`)
	require.NoError(t, err)
	domOnly := &rodSearchSource{page: p}
	_, err = waitFirstCollectable(context.Background(), domOnly, 60*time.Millisecond, 5*time.Millisecond)
	require.ErrorIs(t, err, ErrSearchResultUnconfirmed)
	require.ErrorContains(t, err, "stage=first_collectable_batch")
	require.ErrorContains(t, err, "dom_notes=1", "DOM cards must not be treated as collectable data")

	require.NoError(t, p.Close())
	pageClosed = true
	require.NoError(t, b.Close())
	closed = true
}

// evalJSONInt 读一个返回数字的 JS 表达式。
func evalJSONInt(t *testing.T, p *rod.Page, js string) int {
	t.Helper()
	res, err := p.Eval(js)
	require.NoError(t, err)
	var n int
	require.NoError(t, json.Unmarshal([]byte(res.Value.Str()), &n))
	return n
}
