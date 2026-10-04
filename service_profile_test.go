package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

func TestUserProfileResponseJSONIncludesVerifiedDirectUID(t *testing.T) {
	response := UserProfileResponse{
		UserID:        "0123456789abcdef01234567",
		UserBasicInfo: xiaohongshu.UserBasicInfo{Nickname: "verified profile"},
		Interactions:  []xiaohongshu.UserInteractions{{Type: "fans", Name: "粉丝", Count: "12"}},
		Feeds:         []xiaohongshu.Feed{{ID: "note-1", ModelType: "note"}},
	}

	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	require.JSONEq(t, `{"user_id":"0123456789abcdef01234567","userBasicInfo":{"gender":0,"ipLocation":"","desc":"","imageb":"","nickname":"verified profile","images":"","redId":""},"interactions":[{"type":"fans","name":"粉丝","count":"12"}],"feeds":[{"xsecToken":"","id":"note-1","modelType":"note","noteCard":{"type":"","displayTitle":"","user":{"userId":"","nickname":"","nickName":"","avatar":""},"interactInfo":{"liked":false,"likedCount":"","sharedCount":"","commentCount":"","collectedCount":"","collected":false},"cover":{"width":0,"height":0,"url":"","fileId":"","urlPre":"","urlDefault":"","infoList":null}},"index":0}]}`, string(encoded))
}

func TestUserProfileResponseJSONOmitsEmptyGetMyProfileUID(t *testing.T) {
	response := UserProfileResponse{
		UserBasicInfo: xiaohongshu.UserBasicInfo{Nickname: "my profile"},
		Interactions:  []xiaohongshu.UserInteractions{{Type: "follows", Name: "关注", Count: "3"}},
		Feeds:         []xiaohongshu.Feed{{ID: "my-note", ModelType: "note"}},
	}

	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	require.JSONEq(t, `{"userBasicInfo":{"gender":0,"ipLocation":"","desc":"","imageb":"","nickname":"my profile","images":"","redId":""},"interactions":[{"type":"follows","name":"关注","count":"3"}],"feeds":[{"xsecToken":"","id":"my-note","modelType":"note","noteCard":{"type":"","displayTitle":"","user":{"userId":"","nickname":"","nickName":"","avatar":""},"interactInfo":{"liked":false,"likedCount":"","sharedCount":"","commentCount":"","collectedCount":"","collected":false},"cover":{"width":0,"height":0,"url":"","fileId":"","urlPre":"","urlDefault":"","infoList":null}},"index":0}]}`, string(encoded))
	require.NotContains(t, string(encoded), `"user_id"`)
}

func TestRedactProfileTokenAtReturnedErrorBoundary(t *testing.T) {
	token := "private token+/="
	err := "navigation failed: " + token + " and " + url.QueryEscape(token)
	got := redactProfileToken(err, token)
	require.NotContains(t, got, token)
	require.NotContains(t, got, url.QueryEscape(token))
	require.Equal(t, 2, strings.Count(got, "[redacted]"))
}
