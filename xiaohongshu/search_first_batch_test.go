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
