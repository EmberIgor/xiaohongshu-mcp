package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUserProfileMCPRoutesAndRejectsBadUIDBeforeBrowser(t *testing.T) {
	app := &AppServer{xiaohongshuService: NewXiaohongshuService()}
	result := app.handleUserProfile(context.Background(), map[string]any{"user_id": "not-an-xhs-uid"})
	require.True(t, result.IsError)
	require.Contains(t, result.Content[0].Text, "invalid profile user_id")
}

func TestRESTUserProfileRoutesDirectURLValidationBeforeBrowser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []string{
		`{"profile_url":"https://evil.example/user/profile/0123456789abcdef01234567"}`,
		`{"user_id":"0123456789abcdef01234567","xsec_token":"secret-value","profile_url":"https://www.xiaohongshu.com/user/profile/fedcba9876543210fedcba98"}`,
	} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/user/profile", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		(&AppServer{}).userProfileHandler(ctx)
		require.Equal(t, http.StatusBadRequest, recorder.Code)
		require.NotContains(t, recorder.Body.String(), "secret-value")
	}
}
