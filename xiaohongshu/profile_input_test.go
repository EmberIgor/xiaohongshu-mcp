package xiaohongshu

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveProfileInput(t *testing.T) {
	tests := []struct {
		name, uid, token, url, wantUID, wantToken string
		wantErr                                   bool
	}{
		{"bare id", "0123456789abcdef01234567", "", "", "0123456789abcdef01234567", "", false},
		{"canonical", "", "", "https://www.xiaohongshu.com/user/profile/0123456789abcdef01234567?xsec_token=url-token", "0123456789abcdef01234567", "url-token", false},
		{"matching explicit token", "0123456789abcdef01234567", "explicit-token", "https://www.xiaohongshu.com/user/profile/0123456789abcdef01234567", "0123456789abcdef01234567", "explicit-token", false},
		{"canonical app share tracking", "", "", "https://www.xiaohongshu.com/user/profile/0123456789abcdef01234567?xsec_token=own&xsec_source=app_share&share_from_user_hidden=true&xhsshare=CopyLink", "0123456789abcdef01234567", "own", false},
		{"id conflict", "fedcba9876543210fedcba98", "", "https://www.xiaohongshu.com/user/profile/0123456789abcdef01234567", "", "", true},
		{"token conflict", "0123456789abcdef01234567", "one", "https://www.xiaohongshu.com/user/profile/0123456789abcdef01234567?xsec_token=two", "", "", true},
		{"note URL", "", "", "https://www.xiaohongshu.com/explore/0123456789abcdef", "", "", true},
		{"extra path separator", "", "", "https://www.xiaohongshu.com//user/profile/abcDEF_123-xy/", "", "", true},
		{"encoded identifier", "", "", "https://www.xiaohongshu.com/user/profile/%61bcDEF_123-xy", "", "", true},
		{"credentials", "", "", "https://user:pass@www.xiaohongshu.com/user/profile/abcDEF_123-xy", "", "", true},
		{"port", "", "", "https://www.xiaohongshu.com:8443/user/profile/abcDEF_123-xy", "", "", true},
		{"unsupported scheme", "", "", "http://www.xiaohongshu.com/user/profile/abcDEF_123-xy", "", "", true},
		{"unsupported host", "", "", "https://evil.example/user/profile/abcDEF_123-xy", "", "", true},
		{"missing both", "", "", "", "", "", true},
		{"URL token control character", "", "", "https://www.xiaohongshu.com/user/profile/abcDEF_123-xy?xsec_token=%0A", "", "", true},
		{"malformed query escape", "", "", "https://www.xiaohongshu.com/user/profile/abcDEF_123-xy?xsec_token=%ZZ", "", "", true},
		{"malformed unrelated query escape", "", "", "https://www.xiaohongshu.com/user/profile/0123456789abcdef01234567?share_id=%ZZ", "", "", true},
		{"ignored tab query", "", "", "https://www.xiaohongshu.com/user/profile/abcDEF_123-xy?tab=fav", "", "", true},
		{"malformed uid", "bad/uid", "", "", "", "", true},
		{"uid is not xhs uid", "abcDEF_123-xy", "", "", "", "", true},
		{"uppercase uid is not normalized", "0123456789ABCDEF01234567", "", "", "", "", true},
		{"uid whitespace", " 0123456789abcdef01234567", "", "", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveProfileInput(context.Background(), tt.uid, tt.token, tt.url)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantUID, got.UserID)
			require.Equal(t, tt.wantToken, got.XsecToken)
		})
	}
}

func TestParseCanonicalProfileRejectsDuplicateSource(t *testing.T) {
	u, err := url.Parse("https://www.xiaohongshu.com/user/profile/0123456789abcdef01234567?xsec_source=app&xsec_source=web")
	require.NoError(t, err)
	_, _, err = parseCanonicalProfile(u)
	require.Error(t, err)
}

func TestResolverAllowsOnlyPublicDestinationAddresses(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "169.254.1.1", "::1", "fc00::1", "fe80::1", "224.0.0.1"} {
		require.False(t, isPublicResolverIP(net.ParseIP(raw)), raw)
	}
	require.True(t, isPublicResolverIP(net.ParseIP("1.1.1.1")))
}

func TestResolveProfileShareRedirectIsBoundedAndAllowlisted(t *testing.T) {
	share, err := url.Parse("https://xhslink.cn/a")
	require.NoError(t, err)
	hits := 0
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits++
		require.Equal(t, http.MethodGet, r.Method)
		require.Contains(t, r.Header.Get("User-Agent"), "Android")
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://evil.example/user/profile/abcDEF_123-xy"}}, Body: nopBody{}}, nil
	})}
	_, err = resolveShareURLWithClient(context.Background(), share, client)
	require.Error(t, err)
	require.Equal(t, 1, hits, "untrusted redirect target must never be requested")
	require.NotContains(t, err.Error(), "token-secret")
}

func TestResolveProfileShareRedirectChainAndLoopLimit(t *testing.T) {
	start, _ := url.Parse("https://xhslink.com/a")
	hops := 0
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hops++
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"/again"}}, Body: nopBody{}}, nil
	})}
	_, err := resolveShareURLWithClient(context.Background(), start, client)
	require.Error(t, err)
	require.Equal(t, profileResolveMaxRedirects+1, hops)
}

func TestResolveProfileShareRedirectExtractsOnlyItsOwnToken(t *testing.T) {
	start, _ := url.Parse("https://xhslink.cn/share")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://www.xiaohongshu.com/user/profile/0123456789abcdef01234567?xsec_token=share-owned&share_from_user_hidden=true"}}, Body: nopBody{}}, nil
	})}
	got, err := resolveShareURLWithClient(context.Background(), start, client)
	require.NoError(t, err)
	require.Equal(t, "0123456789abcdef01234567", got.UserID)
	require.Equal(t, "share-owned", got.XsecToken)
}

func TestResolveProfileCancellationBeforeRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ResolveProfileInput(ctx, "", "", "https://xhslink.cn/a")
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "context canceled"))
}

func TestResolverInFlightCancellationBodyBoundAndSafeChain(t *testing.T) {
	start, _ := url.Parse("https://xhslink.cn/share")
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	done := make(chan error, 1)
	go func() { _, err := resolveShareURLWithClient(ctx, start, client); done <- err }()
	<-entered
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)

	var read int
	var bodyClosed bool
	body := &countingBody{remaining: profileResolveMaxBody * 4, read: &read, closed: &bodyClosed}
	resp := &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://www.xiaohongshu.com/user/profile/0123456789abcdef01234567"}}, Body: body}
	bodyClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return resp, nil })}
	_, _ = resolveShareURLWithClient(context.Background(), start, bodyClient)
	require.LessOrEqual(t, read, profileResolveMaxBody)
	require.True(t, bodyClosed)

	hops := 0
	chain := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hops++
		if r.URL.Host == "xhslink.cn" {
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://xhslink.com/next"}}, Body: nopBody{}}, nil
		}
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://www.xiaohongshu.com/user/profile/0123456789abcdef01234567?xsec_token=chain-token"}}, Body: nopBody{}}, nil
	})}
	got, err := resolveShareURLWithClient(context.Background(), start, chain)
	require.NoError(t, err)
	require.Equal(t, "0123456789abcdef01234567", got.UserID)
	require.Equal(t, "chain-token", got.XsecToken)
	require.Equal(t, 2, hops)
}

func TestResolverRejectsUnsafePortAndLoop(t *testing.T) {
	for _, target := range []string{"https://xhslink.cn:8443/next", "https://127.0.0.1/"} {
		start, _ := url.Parse("https://xhslink.com/start")
		hits := 0
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			hits++
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{target}}, Body: nopBody{}}, nil
		})}
		_, err := resolveShareURLWithClient(context.Background(), start, client)
		require.Error(t, err)
		require.Equal(t, 1, hits)
	}
}

func TestResolverUsesRealLoopbackHTTPRedirectFixture(t *testing.T) {
	var hosts []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts = append(hosts, r.Host)
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "https://xhslink.com/second", http.StatusFound)
			return
		}
		http.Redirect(w, r, "https://www.xiaohongshu.com/user/profile/0123456789abcdef01234567?xsec_token=loopback-owned", http.StatusFound)
	}))
	defer server.Close()
	loopback := server.Listener.Addr().String()
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test-only cert from httptest; requests are pinned to this loopback listener
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, loopback)
			},
		},
	}
	start, err := url.Parse("https://xhslink.cn/start")
	require.NoError(t, err)
	got, err := resolveShareURLWithClient(context.Background(), start, client)
	require.NoError(t, err)
	require.Equal(t, "0123456789abcdef01234567", got.UserID)
	require.Equal(t, "loopback-owned", got.XsecToken)
	require.Equal(t, []string{"xhslink.cn", "xhslink.com"}, hosts)
}

func TestResolverInjectedClientMustNotFollowUnsafeRedirect(t *testing.T) {
	start, _ := url.Parse("https://xhslink.cn/start")
	targetHits := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "xhslink.cn" {
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://evil.example/profile"}}, Body: nopBody{}}, nil
		}
		targetHits++
		return &http.Response{StatusCode: http.StatusOK, Body: nopBody{}}, nil
	})}
	_, err := resolveShareURLWithClient(context.Background(), start, client)
	require.Error(t, err)
	require.Zero(t, targetHits, "unsafe redirect destination must never be requested")
}

type countingBody struct {
	remaining int
	read      *int
	closed    *bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > b.remaining {
		n = b.remaining
	}
	b.remaining -= n
	*b.read += n
	return n, nil
}
func (b *countingBody) Close() error {
	if b.closed != nil {
		*b.closed = true
	}
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type nopBody struct{}

func (nopBody) Read([]byte) (int, error) { return 0, io.EOF }
func (nopBody) Close() error             { return nil }
