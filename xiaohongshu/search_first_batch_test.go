package xiaohongshu

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeCollectableSource 复刻 waitFirstCollectable 需要的两个动作：不消费的 probe 与
// 消费式 read。probes 里第 i 项是第 i 次 probe 返回的可采集条数（越界则沿用最后一项）。
type fakeCollectableSource struct {
	probes   []int
	probeIdx int
	probeErr error
	readErr  error
	readFeed []Feed
	reads    int
}

func (f *fakeCollectableSource) probe() (searchProbe, error) {
	if f.probeErr != nil {
		return searchProbe{}, f.probeErr
	}
	if len(f.probes) == 0 {
		return searchProbe{}, nil
	}
	i := f.probeIdx
	if i >= len(f.probes) {
		i = len(f.probes) - 1
	}
	f.probeIdx++
	n := f.probes[i]
	return searchProbe{Collectable: n, HasInitialState: true, Batches: n}, nil
}

func (f *fakeCollectableSource) read() ([]Feed, error) {
	f.reads++
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.readFeed, nil
}

const (
	testBudget = 200 * time.Millisecond
	testPoll   = 5 * time.Millisecond
)

// 探针第一次就说有数据：立刻返回，不空等预算，且真读恰好一次。
func TestWaitFirstCollectableReturnsImmediately(t *testing.T) {
	src := &fakeCollectableSource{probes: []int{2}, readFeed: []Feed{feed("a"), feed("b")}}
	start := time.Now()
	got, err := waitFirstCollectable(context.Background(), src, testBudget, testPoll)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, 1, src.reads)
	require.Equal(t, 1, src.probeIdx)
	require.Less(t, time.Since(start), testBudget/2, "should not burn the whole budget when data is there")
}

// 数据晚到：在预算内等到，成功返回。
func TestWaitFirstCollectableWaitsForDelayedData(t *testing.T) {
	src := &fakeCollectableSource{probes: []int{0, 0, 1}, readFeed: []Feed{feed("late")}}
	got, err := waitFirstCollectable(context.Background(), src, time.Second, testPoll)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "late", got[0].ID)
	require.Equal(t, 3, src.probeIdx)
	require.Equal(t, 1, src.reads)
}

// 探针一直说没有可采集数据：必须是可识别的「未确认」，绝不能当成成功的空结果。
// 同时验证消费纪律：探针说 0 时一次真读都不该发生。
func TestWaitFirstCollectableUnconfirmed(t *testing.T) {
	src := &fakeCollectableSource{probes: []int{0}}
	_, err := waitFirstCollectable(context.Background(), src, 40*time.Millisecond, testPoll)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSearchResultUnconfirmed)
	require.ErrorContains(t, err, "stage=first_collectable_batch")
	require.ErrorContains(t, err, "initial_state=true")
	require.ErrorContains(t, err, "collectable=0")
	require.Zero(t, src.reads, "未确认路径不得消费捕获队列")
}

// 探针报错（JS 错误 / 页面销毁）：原样返回，不伪装成未确认。
func TestWaitFirstCollectableProbeError(t *testing.T) {
	src := &fakeCollectableSource{probeErr: errors.New("js boom")}
	_, err := waitFirstCollectable(context.Background(), src, testBudget, testPoll)
	require.ErrorContains(t, err, "js boom")
	require.ErrorContains(t, err, "stage=first_collectable_batch")
	require.NotErrorIs(t, err, ErrSearchResultUnconfirmed)
	require.Zero(t, src.reads)
}

// 真读报错：原样返回，不伪装成未确认。
func TestWaitFirstCollectableReadError(t *testing.T) {
	src := &fakeCollectableSource{probes: []int{1}, readErr: errors.New("read boom")}
	_, err := waitFirstCollectable(context.Background(), src, testBudget, testPoll)
	require.ErrorContains(t, err, "read boom")
	require.NotErrorIs(t, err, ErrSearchResultUnconfirmed)
}

// 父 context 已取消：必须直接返回取消，而不是等到预算耗尽再报未确认。
func TestWaitFirstCollectableContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := &fakeCollectableSource{probes: []int{0}}
	_, err := waitFirstCollectable(ctx, src, testBudget, testPoll)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrSearchResultUnconfirmed)
}

// 父 deadline 比首批预算更短：按父 deadline 失败。
func TestWaitFirstCollectableParentDeadlineWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	src := &fakeCollectableSource{probes: []int{0}}
	_, err := waitFirstCollectable(ctx, src, 5*time.Second, testPoll)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, ErrSearchResultUnconfirmed)
}

// 探针说有、真读却拿到空（极小概率的页面变化）：不得当成成功，继续等到预算耗尽后报未确认。
func TestWaitFirstCollectableProbeReadMismatchStaysUnconfirmed(t *testing.T) {
	src := &fakeCollectableSource{probes: []int{1}, readFeed: nil}
	_, err := waitFirstCollectable(context.Background(), src, 40*time.Millisecond, testPoll)
	require.ErrorIs(t, err, ErrSearchResultUnconfirmed)
	require.GreaterOrEqual(t, src.reads, 1)
}

// 与真实快照解码共用同一套规则：非 note / 无 id 不算可采集（searchSnapshot.decode）。
func TestSearchSnapshotDecodeIgnoresNonNotes(t *testing.T) {
	empty := searchSnapshot{}
	got, err := empty.decode(true)
	require.NoError(t, err)
	require.Empty(t, got, "空快照不得产出任何可采集笔记")

	snap := searchSnapshot{
		Batches: []json.RawMessage{json.RawMessage(`{"data":{"items":[{"id":"x","model_type":"hot_query"}]}}`)},
	}
	got, err = snap.decode(true)
	require.NoError(t, err)
	require.Empty(t, got, "只有非 note 项的批次不算可采集数据")

	snap = searchSnapshot{
		State:   json.RawMessage(`[]`),
		Batches: []json.RawMessage{json.RawMessage(`{"data":{"items":[{"id":"n1","model_type":"note"}]}}`)},
	}
	got, err = snap.decode(true)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "n1", got[0].ID)

	// 有批次时不得再用可能过期的 IS 快照。
	snap.State = json.RawMessage(`[{"id":"stale","modelType":"note"}]`)
	got, err = snap.decode(true)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "n1", got[0].ID)

	// 没有批次时才读 IS（裸数组形态）。
	snap.Batches = nil
	got, err = snap.decode(true)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "stale", got[0].ID)

	// 筛选后（skipInitialState）不得读 IS。
	got, err = snap.decode(false)
	require.NoError(t, err)
	require.Empty(t, got)
}

// Matrix exercises the business outcome after the snapshot's JS-equivalent wrapper extraction.
// The browser fixture additionally verifies the actual JS extraction.
func TestFirstBatchQualificationOutcomeMatrix(t *testing.T) {
	for _, shape := range []string{"array", "value", "_value"} {
		for _, tc := range []struct {
			name, items string
			valid       bool
		}{
			{"missing_id", `[{"modelType":"note"}]`, false},
			{"empty_id", `[{"id":"","modelType":"note"}]`, false},
			{"non_note", `[{"id":"x","modelType":"live_v2"}]`, false},
			{"mixed", `[{"modelType":"note"},{"id":"","modelType":"note"},{"id":"x","modelType":"hot_query"},{"id":"good","modelType":"note"}]`, true},
		} {
			t.Run(shape+"/"+tc.name, func(t *testing.T) {
				raw := json.RawMessage(tc.items)
				if shape != "array" {
					wrapped := []byte(`{"` + shape + `":` + tc.items + `}`)
					var fields map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(wrapped, &fields))
					raw = fields[shape]
				}
				fs, err := (searchSnapshot{State: raw}).decode(true)
				require.NoError(t, err)
				out, err := collectSearch(context.Background(), &fakeSearchSource{batches: [][]Feed{fs}}, SearchScrollConfig{})
				if !tc.valid {
					require.ErrorIs(t, err, ErrSearchResultUnconfirmed)
					require.Empty(t, out.Feeds)
					require.ErrorContains(t, err, "terminal_assertion")
				} else {
					require.NoError(t, err)
					require.Equal(t, []Feed{feed("good")}, out.Feeds)
					require.Equal(t, 1, out.Metadata.InitialCount)
				}
				require.Equal(t, "max_scrolls_reached", out.Metadata.StopReason)
			})
		}
	}
}

type cancelOnReadSource struct {
	fakeCollectableSource
	cancel context.CancelFunc
}

func (s *cancelOnReadSource) read() ([]Feed, error) {
	s.cancel()
	return s.fakeCollectableSource.read()
}
func TestFirstBatchCancellationWithAvailableData(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := &fakeCollectableSource{probes: []int{1}, readFeed: []Feed{feed("a")}}
	got, err := waitFirstCollectable(ctx, src, testBudget, testPoll)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, got)
	require.Zero(t, src.probeIdx)
	require.Zero(t, src.reads)

	ctx, cancel = context.WithCancel(context.Background())
	during := &cancelOnReadSource{fakeCollectableSource: *src, cancel: cancel}
	got, err = waitFirstCollectable(ctx, during, testBudget, testPoll)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, got)
}
func TestFirstBatchBudgetClampsSleep(t *testing.T) {
	const budget = 20 * time.Millisecond
	const poll = 250 * time.Millisecond
	src := &fakeCollectableSource{probes: []int{0, 1}, readFeed: []Feed{feed("late")}}
	start := time.Now()
	got, err := waitFirstCollectable(context.Background(), src, budget, poll)
	elapsed := time.Since(start)
	require.ErrorIs(t, err, ErrSearchResultUnconfirmed)
	require.Empty(t, got)
	require.LessOrEqual(t, elapsed-budget, poll)
	require.Equal(t, 1, src.probeIdx, "must abandon before starting a post-budget probe")
	require.ErrorContains(t, err, "budget=")
	require.ErrorContains(t, err, "elapsed=")
}

// A single in-flight read may exceed budget; budget is not an I/O deadline.
type slowReadSource struct{ fakeCollectableSource }

func (s *slowReadSource) read() ([]Feed, error) {
	time.Sleep(30 * time.Millisecond)
	return s.fakeCollectableSource.read()
}
func TestFirstBatchAllowsInFlightReadBeyondBudget(t *testing.T) {
	src := &slowReadSource{fakeCollectableSource{probes: []int{1}, readFeed: []Feed{feed("a")}}}
	got, err := waitFirstCollectable(context.Background(), src, time.Millisecond, testPoll)
	require.NoError(t, err)
	require.Equal(t, []Feed{feed("a")}, got)
}
func TestCollectSearchTerminalAssertionCoversPartialExit(t *testing.T) {
	src := &fakeSearchSource{signalErrAt: 2}
	out, err := collectSearch(context.Background(), src, SearchScrollConfig{MaxScrolls: 1})
	require.ErrorIs(t, err, ErrSearchResultUnconfirmed)
	require.Equal(t, "risk_control", out.Metadata.StopReason)
	require.ErrorContains(t, err, "terminal_assertion=zero_collectable_notes stop_reason=risk_control")
	require.Empty(t, out.Feeds)
}
