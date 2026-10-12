package xiaohongshu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type SearchMetadata struct {
	InitialCount     int    `json:"initial_count"`
	ScrollsPerformed int    `json:"scrolls_performed"`
	StopReason       string `json:"stop_reason"`
}
type SearchOutcome struct {
	Feeds    []Feed
	Metadata SearchMetadata
}

type searchSource interface {
	read() ([]Feed, error)
	scroll() error
	wait(context.Context, time.Duration) error
	signal() error
}

func collectSearch(ctx context.Context, src searchSource, cfg SearchScrollConfig) (SearchOutcome, error) {
	return collectSearchWithTiming(ctx, src, cfg, 8*time.Second, 250*time.Millisecond)
}
func collectSearchWithTiming(ctx context.Context, src searchSource, cfg SearchScrollConfig, roundTimeout, poll time.Duration) (result SearchOutcome, resultErr error) {
	// 所有成功出口共用一处终局断言；不改变部分结果的 StopReason。
	defer func() {
		if resultErr != nil {
			return
		}
		for _, f := range result.Feeds {
			if isCollectableNote(f) {
				return
			}
		}
		resultErr = fmt.Errorf("%w: stage=search_outcome terminal_assertion=zero_collectable_notes stop_reason=%s", ErrSearchResultUnconfirmed, result.Metadata.StopReason)
	}()
	a := searchAccumulator{}
	if err := src.signal(); err != nil {
		return SearchOutcome{}, err
	}
	batch, err := src.read()
	if err != nil {
		return SearchOutcome{}, err
	}
	a.add(batch)
	out := SearchOutcome{Metadata: SearchMetadata{InitialCount: a.initialCount(), StopReason: "max_scrolls_reached"}}
	if cfg.Limit > 0 && len(a.items) >= cfg.Limit {
		out.Metadata.StopReason = "limit_reached"
	}
	empty := 0
	for out.Metadata.ScrollsPerformed < cfg.MaxScrolls && out.Metadata.StopReason != "limit_reached" {
		if err := ctx.Err(); err != nil {
			out.Metadata.StopReason = searchContextReason(err)
			break
		}
		if err := src.signal(); err != nil {
			if ctx.Err() != nil {
				out.Metadata.StopReason = searchContextReason(ctx.Err())
				return SearchOutcome{Feeds: a.feeds(), Metadata: out.Metadata}, nil
			}
			out.Metadata.StopReason = signalReason(err)
			return SearchOutcome{Feeds: a.feeds(), Metadata: out.Metadata}, nil
		}
		if err := src.scroll(); err != nil {
			if ctx.Err() != nil {
				out.Metadata.StopReason = searchContextReason(ctx.Err())
				return SearchOutcome{Feeds: a.feeds(), Metadata: out.Metadata}, nil
			}
			out.Metadata.StopReason = "browser_error"
			return SearchOutcome{Feeds: a.feeds(), Metadata: out.Metadata}, nil
		}
		out.Metadata.ScrollsPerformed++
		changed := false
		deadline := time.Now().Add(roundTimeout)
		for time.Now().Before(deadline) {
			if err := ctx.Err(); err != nil {
				out.Metadata.StopReason = searchContextReason(err)
				break
			}
			if err := src.signal(); err != nil {
				if ctx.Err() != nil {
					out.Metadata.StopReason = searchContextReason(ctx.Err())
					return SearchOutcome{Feeds: a.feeds(), Metadata: out.Metadata}, nil
				}
				out.Metadata.StopReason = signalReason(err)
				return SearchOutcome{Feeds: a.feeds(), Metadata: out.Metadata}, nil
			}
			next, err := src.read()
			if err != nil {
				if ctx.Err() != nil {
					out.Metadata.StopReason = searchContextReason(ctx.Err())
					break
				}
				out.Metadata.StopReason = "browser_error"
				return SearchOutcome{Feeds: a.feeds(), Metadata: out.Metadata}, nil
			}
			if a.add(next) > 0 {
				changed = true
				break
			}
			if err := src.wait(ctx, poll); err != nil {
				out.Metadata.StopReason = searchContextReason(err)
				break
			}
		}
		if out.Metadata.StopReason == "context_canceled" || out.Metadata.StopReason == "timeout" {
			break
		}
		if changed {
			empty = 0
		} else {
			empty++
			if empty >= 2 {
				out.Metadata.StopReason = "no_new_results"
			}
		}
		if cfg.Limit > 0 && len(a.items) >= cfg.Limit {
			out.Metadata.StopReason = "limit_reached"
		}
		if out.Metadata.StopReason == "no_new_results" || out.Metadata.StopReason == "limit_reached" {
			break
		}
	}
	if cfg.Limit > 0 && len(a.items) > cfg.Limit {
		a.items = a.items[:cfg.Limit]
	}
	out.Feeds = a.feeds()
	return out, nil
}
func searchContextReason(err error) string {
	if err == context.DeadlineExceeded {
		return "timeout"
	}
	return "context_canceled"
}
func signalReason(err error) string {
	s := err.Error()
	if strings.Contains(s, "risk_control:") {
		return "risk_control"
	}
	if strings.Contains(s, "login_required:") {
		return "login_required"
	}
	return "browser_error"
}

// normalizeSearchItems handles the API's snake_case envelope and preserves the complete Feed.
func normalizeSearchItems(payload []byte) ([]Feed, error) {
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, err
	}
	data, _ := root["data"].(map[string]any)
	items, _ := data["items"].([]any)
	out := make([]Feed, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		b, err := json.Marshal(camelizeMap(m))
		if err != nil {
			return nil, err
		}
		var f Feed
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, err
		}
		if isCollectableNote(f) {
			out = append(out, f)
		}
	}
	return out, nil
}

func camelizeMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		var b strings.Builder
		upper := false
		for _, r := range k {
			if r == '_' {
				upper = true
				continue
			}
			if upper {
				if r >= 'a' && r <= 'z' {
					r -= 32
				}
				upper = false
			}
			b.WriteRune(r)
		}
		switch x := v.(type) {
		case map[string]any:
			v = camelizeMap(x)
		case []any:
			a := make([]any, len(x))
			for i, e := range x {
				if m, ok := e.(map[string]any); ok {
					a[i] = camelizeMap(m)
				} else {
					a[i] = e
				}
			}
			v = a
		}
		out[b.String()] = v
	}
	return out
}

type searchAccumulator struct {
	items   []Feed
	seen    map[string]bool
	initial int
	first   bool
}

func (a *searchAccumulator) add(batch []Feed) int {
	if !a.first {
		a.first = true
		ids := map[string]bool{}
		for _, f := range batch {
			if isCollectableNote(f) {
				ids[f.ID] = true
			}
		}
		a.initial = len(ids)
	}
	if a.seen == nil {
		a.seen = map[string]bool{}
	}
	before := len(a.items)
	for _, f := range batch {
		if !isCollectableNote(f) || a.seen[f.ID] {
			continue
		}
		a.seen[f.ID] = true
		a.items = append(a.items, f)
	}
	return len(a.items) - before
}
func (a *searchAccumulator) initialCount() int { return a.initial }
func (a *searchAccumulator) feeds() []Feed {
	if a.items == nil {
		return []Feed{}
	}
	return a.items
}

const searchCaptureHook = `(() => {
 const batches = window.__xhsSearchBatches = window.__xhsSearchBatches || [];
 const capture = (url,text) => { try { const u=new URL(String(url),location.href);if(!/\/search\//i.test(u.pathname)&&!/\/search\?/i.test(u.pathname))return; const x=typeof text==='string'?JSON.parse(text):text; if(x && x.data && Array.isArray(x.data.items)){batches.push(x);if(batches.length>8)batches.splice(0,batches.length-8)} } catch(_){} };
 const f=window.fetch; window.fetch=function(...a){return f.apply(this,a).then(r=>{try{r.clone().text().then(t=>capture(a[0]?.url||a[0],t)).catch(()=>{})}catch(_){};return r})};
 const open=XMLHttpRequest.prototype.open, send=XMLHttpRequest.prototype.send;
 XMLHttpRequest.prototype.open=function(...a){this.__xhsURL=String(a[1]||'');return open.apply(this,a)};
 XMLHttpRequest.prototype.send=function(...a){this.addEventListener('load',function(){try{capture(this.__xhsURL,this.responseType==='json'?this.response:this.responseText)}catch(_){}});return send.apply(this,a)};
})();`
