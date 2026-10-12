package xiaohongshu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/devices"
	"github.com/go-rod/rod/lib/proto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Only the browser transport is replaced: probe/read, snapshot decoding, stateRead,
// primed delivery and collection all execute the production implementations.
// This does not execute JS or verify wrapper extraction (the browser fixture does).
type firstBatchSnapshotCDP struct {
	snapshot searchSnapshot
	reads    int
}

func (c *firstBatchSnapshotCDP) Event() <-chan *cdp.Event { return make(chan *cdp.Event) }
func (c *firstBatchSnapshotCDP) Call(_ context.Context, _, method string, params interface{}) ([]byte, error) {
	switch method {
	case "Target.attachToTarget":
		return []byte(`{"sessionId":"fixture"}`), nil
	case "Runtime.evaluate":
		return []byte(`{"result":{"type":"object","objectId":"window"}}`), nil
	case "Runtime.callFunctionOn":
		req := params.(proto.RuntimeCallFunctionOn)
		js := req.FunctionDeclaration
		switch {
		case strings.Contains(js, "const captured = batches."):
			requireConsume := strings.Contains(js, "batches.splice(0)")
			raw, err := json.Marshal(c.snapshot)
			if err != nil {
				return nil, err
			}
			if requireConsume {
				c.reads++
				c.snapshot.Batches = nil
			}
			return json.Marshal(map[string]interface{}{"result": map[string]interface{}{"type": "string", "value": string(raw)}})
		case strings.Contains(js, "const risk="):
			return []byte(`{"result":{"type":"object","value":{"url":"/search_result","risk":false,"login":false}}}`), nil
		case strings.Contains(js, "scrollTo"):
			return []byte(`{"result":{"type":"undefined"}}`), nil
		default:
			return nil, fmt.Errorf("unexpected fixture JS: %s", js)
		}
	default:
		return []byte(`{}`), nil
	}
}
func firstBatchRodFixture(t *testing.T, items string) (*rodSearchSource, *firstBatchSnapshotCDP) {
	t.Helper()
	c := &firstBatchSnapshotCDP{snapshot: searchSnapshot{State: json.RawMessage(items), HasInitialState: true}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b := rod.New().Context(ctx).Client(c).DefaultDevice(devices.Clear)
	require.NoError(t, b.Connect())
	p, err := b.PageFromTarget("fixture")
	require.NoError(t, err)
	return &rodSearchSource{page: p}, c
}

var firstBatchInvalidCases = []struct{ name, items string }{
	{"missing_id", `[{"modelType":"note"}]`},
	{"empty_id", `[{"id":"","modelType":"note"}]`},
	{"non_note", `[{"id":"x","modelType":"live_v2"}]`},
	{"invalid_combined", `[{"modelType":"note"},{"id":"","modelType":"note"},{"id":"x","modelType":"live_v2"}]`},
}

const firstBatchMixedIS = `[{"modelType":"note"},{"id":"","modelType":"note"},{"id":"x","modelType":"live_v2"},{"id":"a","modelType":"note"},{"id":"b","modelType":"note"}]`

func TestFirstBatchR3ProbeQualification(t *testing.T) {
	cases := append(append([]struct{ name, items string }{}, firstBatchInvalidCases...), struct{ name, items string }{"mixed", firstBatchMixedIS})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, transport := firstBatchRodFixture(t, tc.items)
			for i := 0; i < 2; i++ {
				p, err := src.probe()
				require.NoError(t, err)
				expected := 0
				if tc.name == "mixed" {
					expected = 2
				}
				require.Equal(t, expected, p.Collectable)
				require.True(t, p.HasInitialState)
				require.Zero(t, p.Batches)
				require.False(t, src.stateRead)
				require.Zero(t, transport.reads)
			}
		})
	}
}
func TestFirstBatchR3InvalidWaitDoesNotConsume(t *testing.T) {
	for _, tc := range firstBatchInvalidCases {
		t.Run(tc.name, func(t *testing.T) {
			src, transport := firstBatchRodFixture(t, tc.items)
			got, err := waitFirstCollectable(context.Background(), src, 15*time.Millisecond, time.Millisecond)
			// Assert discipline before outcome so the terminal guard cannot hide a regression.
			assert.Zero(t, transport.reads, "invalid IS must never trigger a consuming snapshot")
			assert.False(t, src.stateRead)
			require.Empty(t, got)
			require.ErrorIs(t, err, ErrSearchResultUnconfirmed)
		})
	}
}
func TestFirstBatchR3AcceptanceChain(t *testing.T) {
	for _, limit := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			src, transport := firstBatchRodFixture(t, firstBatchMixedIS)
			p, err := src.probe()
			require.NoError(t, err)
			assert.Equal(t, 2, p.Collectable)
			accepted, err := waitFirstCollectable(context.Background(), src, testBudget, testPoll)
			require.NoError(t, err)
			assert.Equal(t, []Feed{feed("a"), feed("b")}, accepted)
			require.Equal(t, 1, transport.reads)
			require.True(t, src.stateRead)
			src.primed = accepted
			out, err := collectSearch(context.Background(), src, SearchScrollConfig{Limit: limit})
			require.NoError(t, err)
			expected := []Feed{feed("a"), feed("b")}
			if limit == 1 {
				expected = expected[:1]
			}
			require.Equal(t, expected, out.Feeds)
			require.Equal(t, 2, out.Metadata.InitialCount)
			reason := "max_scrolls_reached"
			if limit > 0 {
				reason = "limit_reached"
			}
			require.Equal(t, reason, out.Metadata.StopReason)
			require.True(t, src.primedUsed)
			again, err := src.read()
			require.NoError(t, err)
			require.Empty(t, again)
		})
	}
}
