package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

func TestSearchOptionsValidation(t *testing.T) {
	for _, tc := range []struct {
		limit *int
		valid bool
	}{{nil, true}, {ptr(1), true}, {ptr(200), true}, {ptr(0), false}, {ptr(201), false}, {ptr(-1), false}} {
		err := xiaohongshu.ValidateSearchOptions(0, tc.limit)
		require.Equal(t, tc.valid, err == nil)
	}
}

func TestSearchMCPProtocolSchemaAndInvalidCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := InitMCPServer(&AppServer{})
	ct, st := mcp.NewInMemoryTransports()
	_, err := srv.Connect(ctx, st, nil)
	require.NoError(t, err)
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	defer client.Close()
	tools, err := client.ListTools(ctx, nil)
	require.NoError(t, err)
	var found *mcp.Tool
	for i := range tools.Tools {
		if tools.Tools[i].Name == "search_feeds" {
			found = tools.Tools[i]
			break
		}
	}
	require.NotNil(t, found)
	b, err := json.Marshal(found.InputSchema)
	require.NoError(t, err)
	var schema jsonschema.Schema
	require.NoError(t, json.Unmarshal(b, &schema))
	props := schema.Properties
	require.Equal(t, float64(1), *props["limit"].Minimum)
	require.Equal(t, float64(200), *props["limit"].Maximum)
	require.Equal(t, float64(0), *props["max_scrolls"].Minimum)
	require.Equal(t, float64(3), *props["max_scrolls"].Maximum)
	for _, args := range []map[string]any{{"keyword": "x", "limit": float64(0)}, {"keyword": "x", "limit": "2"}, {"keyword": "x", "limit": 1.5}, {"keyword": "x", "max_scrolls": "2"}, {"keyword": "x", "max_scrolls": 1.5}} {
		r, e := client.CallTool(ctx, &mcp.CallToolParams{Name: "search_feeds", Arguments: args})
		if e == nil {
			require.True(t, r.IsError)
		}
	}
}
func ptr(i int) *int { return &i }

func TestSearchHTTPRejectsInvalidLimitBeforeService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct{ method, body, path string }{{"POST", `{"keyword":"k","limit":0}`, ""}, {"POST", `{"keyword":"k","limit":1.5}`, ""}, {"GET", "", "?keyword=k&limit="}} {
		req := httptest.NewRequest(tc.method, "/api/v1/feeds/search"+tc.path, bytes.NewBufferString(tc.body))
		if tc.method == "POST" {
			req.Header.Set("Content-Type", "application/json")
		}
		rw := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rw)
		c.Request = req
		(&AppServer{}).searchFeedsHandler(c)
		require.Equal(t, 400, rw.Code)
	}
}

func TestSearchMCPLimitTypesAndBoundsBeforeService(t *testing.T) {
	for _, raw := range []string{`{"keyword":"k","limit":"2"}`, `{"keyword":"k","limit":1.5}`} {
		var args SearchFeedsArgs
		require.Error(t, json.Unmarshal([]byte(raw), &args))
	}
	for _, n := range []int{0, 201, -2} {
		args := SearchFeedsArgs{Keyword: "k", Limit: &n}
		result := (&AppServer{}).handleSearchFeeds(context.Background(), args)
		require.True(t, result.IsError)
	}
}
