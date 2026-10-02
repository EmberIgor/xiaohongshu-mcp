package xiaohongshu

import (
	"context"
	"errors"
	"fmt"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestSearchResponseSnakeCaseNormalization(t *testing.T) {
	feeds, err := normalizeSearchItems([]byte(`{"data":{"items":[{"id":"n1","model_type":"note","xsec_token":"outer-token","note_card":{"display_title":"title","user":{"user_id":"u1","nickname":"nick","xsec_token":"user-token"},"cover":{"url_default":"cover-url"},"interact_info":{"liked_count":"12"}}},{"id":"x","model_type":"hot_query"}]}}`))
	require.NoError(t, err)
	require.Len(t, feeds, 1)
	require.Equal(t, "note", feeds[0].ModelType)
	require.Equal(t, "outer-token", feeds[0].XsecToken)
	require.Equal(t, "title", feeds[0].NoteCard.DisplayTitle)
	require.Equal(t, "u1", feeds[0].NoteCard.User.UserID)
	require.Equal(t, "nick", feeds[0].NoteCard.User.Nickname)
	require.Equal(t, "user-token", feeds[0].NoteCard.User.XsecToken)
	require.Equal(t, "cover-url", feeds[0].NoteCard.Cover.URLDefault)
	require.Equal(t, "12", feeds[0].NoteCard.InteractInfo.LikedCount)
}

type fakeSearchSource struct {
	batches        [][]Feed
	reads, scrolls int
	signalErrAt    int
	signalErr      string
	readErr        bool
	readErrAt      int
	scrollErr      bool
	waitErr        error
	signals        int
}

func (f *fakeSearchSource) read() ([]Feed, error) {
	f.reads++
	if f.readErr || f.readErrAt == f.reads {
		return nil, errors.New("read failed")
	}
	if len(f.batches) == 0 {
		return []Feed{}, nil
	}
	i := f.reads - 1
	if i >= len(f.batches) {
		i = len(f.batches) - 1
	}
	return f.batches[i], nil
}
func (f *fakeSearchSource) scroll() error {
	f.scrolls++
	if f.scrollErr {
		return errors.New("scroll failed")
	}
	return nil
}
func (f *fakeSearchSource) wait(ctx context.Context, _ time.Duration) error {
	if f.waitErr != nil {
		return f.waitErr
	}
	return ctx.Err()
}
func (f *fakeSearchSource) signal() error {
	f.signals++
	if f.signalErrAt == f.signals {
		if f.signalErr == "" {
			f.signalErr = "risk_control"
		}
		return fmt.Errorf("%s: challenge", f.signalErr)
	}
	return nil
}
func feed(id string) Feed { return Feed{ID: id, ModelType: modelTypeNote} }

func TestCollectSearchInitialOnly(t *testing.T) {
	src := &fakeSearchSource{batches: [][]Feed{{feed("a")}}}
	o, e := collectSearch(context.Background(), src, SearchScrollConfig{})
	require.NoError(t, e)
	require.Equal(t, 1, o.Metadata.InitialCount)
	require.Equal(t, 0, o.Metadata.ScrollsPerformed)
	require.Equal(t, "max_scrolls_reached", o.Metadata.StopReason)
}
func TestCollectSearchMaxThreeAndAddsNewResults(t *testing.T) {
	src := &fakeSearchSource{batches: [][]Feed{{feed("a")}, {feed("a"), feed("b")}, {feed("b"), feed("c")}, {feed("d")}}}
	o, e := collectSearch(context.Background(), src, SearchScrollConfig{MaxScrolls: 3})
	require.NoError(t, e)
	require.Len(t, o.Feeds, 4)
	require.Equal(t, 1, o.Metadata.InitialCount)
	require.Equal(t, 3, o.Metadata.ScrollsPerformed)
	require.Equal(t, "max_scrolls_reached", o.Metadata.StopReason)
}
func TestCollectSearchTwoEmptyRounds(t *testing.T) {
	src := &fakeSearchSource{batches: [][]Feed{{feed("a")}}}
	o, e := collectSearchWithTiming(context.Background(), src, SearchScrollConfig{MaxScrolls: 3}, time.Millisecond, 0)
	require.NoError(t, e)
	require.Equal(t, 2, o.Metadata.ScrollsPerformed)
	require.Equal(t, "no_new_results", o.Metadata.StopReason)
}
func TestCollectSearchLimitAndInitialCountImmutable(t *testing.T) {
	src := &fakeSearchSource{batches: [][]Feed{{feed("a"), feed("b")}, {feed("a"), feed("b"), feed("c")}}}
	o, e := collectSearch(context.Background(), src, SearchScrollConfig{MaxScrolls: 3, Limit: 2})
	require.NoError(t, e)
	require.Len(t, o.Feeds, 2)
	require.Equal(t, 2, o.Metadata.InitialCount)
	require.Equal(t, "limit_reached", o.Metadata.StopReason)
	require.Equal(t, 0, o.Metadata.ScrollsPerformed)
}
func TestCollectSearchCancellationAndDeadline(t *testing.T) {
	for _, tc := range []struct {
		err    error
		reason string
		cancel bool
	}{{context.Canceled, "context_canceled", true}, {context.DeadlineExceeded, "timeout", false}} {
		ctx, cancel := context.WithCancel(context.Background())
		src := &fakeSearchSource{batches: [][]Feed{{feed("a")}}, waitErr: tc.err}
		if tc.cancel {
			cancel()
		} else {
			defer cancel()
		}
		o, e := collectSearch(ctx, src, SearchScrollConfig{MaxScrolls: 1})
		require.NoError(t, e)
		require.Equal(t, tc.reason, o.Metadata.StopReason)
		require.Len(t, o.Feeds, 1)
	}
}
func TestCollectSearchRiskBeforeScrollAndAccept(t *testing.T) {
	beforeScroll := &fakeSearchSource{batches: [][]Feed{{feed("a")}}, signalErrAt: 2}
	pre, e := collectSearch(context.Background(), beforeScroll, SearchScrollConfig{MaxScrolls: 1})
	require.NoError(t, e)
	require.Equal(t, "risk_control", pre.Metadata.StopReason)
	require.Zero(t, beforeScroll.scrolls)
	src := &fakeSearchSource{batches: [][]Feed{{feed("a")}, {feed("b")}}, signalErrAt: 3}
	o, e := collectSearch(context.Background(), src, SearchScrollConfig{MaxScrolls: 1})
	require.NoError(t, e)
	require.Equal(t, "risk_control", o.Metadata.StopReason)
	require.Equal(t, 1, src.scrolls)
	require.Len(t, o.Feeds, 1)
	require.Equal(t, 1, src.reads)
}
func TestCollectSearchBrowserErrors(t *testing.T) {
	src := &fakeSearchSource{batches: [][]Feed{{feed("a")}}, scrollErr: true}
	o, e := collectSearch(context.Background(), src, SearchScrollConfig{MaxScrolls: 1})
	require.NoError(t, e)
	require.Equal(t, "browser_error", o.Metadata.StopReason)
	require.Len(t, o.Feeds, 1)
}

func TestCollectSearchErrorsBeforeInitialAndLoginInterruption(t *testing.T) {
	src := &fakeSearchSource{readErr: true}
	_, err := collectSearch(context.Background(), src, SearchScrollConfig{})
	require.Error(t, err)
	src = &fakeSearchSource{batches: [][]Feed{{feed("a")}}, signalErrAt: 3, signalErr: "login_required"}
	o, err := collectSearch(context.Background(), src, SearchScrollConfig{MaxScrolls: 1})
	require.NoError(t, err)
	require.Equal(t, "login_required", o.Metadata.StopReason)
	require.Len(t, o.Feeds, 1)
}

func TestCollectSearchReadErrorAfterInitialReturnsPartial(t *testing.T) {
	src := &fakeSearchSource{batches: [][]Feed{{feed("a")}}, readErrAt: 2}
	o, err := collectSearch(context.Background(), src, SearchScrollConfig{MaxScrolls: 1})
	require.NoError(t, err)
	require.Equal(t, "browser_error", o.Metadata.StopReason)
	require.Len(t, o.Feeds, 1)
}

func TestAccumulatorKeepsFirstBatchCountAndDeduplicates(t *testing.T) {
	a := searchAccumulator{}
	first, _ := normalizeSearchItems([]byte(`{"data":{"items":[{"id":"n1","model_type":"note"},{"id":"x","model_type":"live_v2"}]}}`))
	require.Equal(t, 1, a.add(first))
	second, _ := normalizeSearchItems([]byte(`{"data":{"items":[{"id":"n1","model_type":"note"},{"id":"n2","model_type":"note"}]}}`))
	require.Equal(t, 1, a.add(second))
	require.Equal(t, 1, a.initialCount())
	require.Len(t, a.feeds(), 2)
}
