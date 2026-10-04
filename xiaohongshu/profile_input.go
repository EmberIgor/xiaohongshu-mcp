package xiaohongshu

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const profileResolveMaxRedirects = 5
const profileResolveMaxBody = 64 << 10
const profileResolveMaxURL = 4096

type ResolvedProfileInput struct {
	UserID     string
	XsecToken  string
	ProfileURL string
}

// ResolveProfileInput validates direct IDs/profile URLs and resolves only the explicitly
// supported share hosts. Every redirect is checked before another request is made.
func ResolveProfileInput(ctx context.Context, userID, token, rawURL string) (ResolvedProfileInput, error) {
	if userID != strings.TrimSpace(userID) || token != strings.TrimSpace(token) {
		return ResolvedProfileInput{}, fmt.Errorf("user_id and xsec_token must not contain surrounding whitespace")
	}
	if userID != "" && !validProfileID(userID) {
		return ResolvedProfileInput{}, fmt.Errorf("invalid profile user_id")
	}
	if len(token) > 2048 || strings.ContainsAny(token, "\r\n\x00") {
		return ResolvedProfileInput{}, fmt.Errorf("invalid xsec_token")
	}
	if rawURL == "" {
		if userID == "" {
			return ResolvedProfileInput{}, fmt.Errorf("user_id or profile_url is required")
		}
		return ResolvedProfileInput{UserID: userID, XsecToken: token}, nil
	}
	u, err := parseSafeProfileURL(rawURL)
	if err != nil {
		return ResolvedProfileInput{}, err
	}
	if u.Hostname() == "www.xiaohongshu.com" {
		parsedID, parsedToken, err := parseCanonicalProfile(u)
		if err != nil {
			return ResolvedProfileInput{}, err
		}
		return mergeProfileIdentity(userID, token, parsedID, parsedToken)
	}
	if u.Hostname() != "xhslink.cn" && u.Hostname() != "xhslink.com" {
		return ResolvedProfileInput{}, fmt.Errorf("unsupported profile URL host")
	}
	resolved, err := resolveShareURL(ctx, u)
	if err != nil {
		return ResolvedProfileInput{}, err
	}
	return mergeProfileIdentity(userID, token, resolved.UserID, resolved.XsecToken)
}

func validProfileID(s string) bool {
	if len(s) != 24 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'f' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func parseSafeProfileURL(raw string) (*url.URL, error) {
	if len(raw) > profileResolveMaxURL {
		return nil, fmt.Errorf("profile_url exceeds maximum length")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Fragment != "" {
		return nil, fmt.Errorf("profile_url must be an HTTPS URL without credentials, port, or fragment")
	}
	host := strings.ToLower(u.Hostname())
	if host != "www.xiaohongshu.com" && host != "xhslink.cn" && host != "xhslink.com" {
		return nil, fmt.Errorf("unsupported profile URL host")
	}
	return u, nil
}

func parseCanonicalProfile(u *url.URL) (string, string, error) {
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "user" || parts[1] != "profile" || !validProfileID(parts[2]) || u.EscapedPath() != "/user/profile/"+parts[2] {
		return "", "", fmt.Errorf("profile_url is not a canonical user profile")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", "", fmt.Errorf("malformed profile URL query")
	}
	allowed := map[string]bool{"xsec_token": true, "xsec_source": true, "share_from_user_hidden": true, "xhsshare": true, "share_id": true, "source": true, "app_platform": true, "apptime": true, "shareRedId": true}
	for k, values := range q {
		if !allowed[k] || len(values) != 1 {
			return "", "", fmt.Errorf("unsupported profile URL query")
		}
		if values[0] == "" && k != "xsec_token" {
			return "", "", fmt.Errorf("invalid profile URL query")
		}
		if len(values[0]) > 512 || strings.ContainsAny(values[0], "\r\n\x00") {
			return "", "", fmt.Errorf("invalid profile URL query")
		}
	}
	tokens := q["xsec_token"]
	if len(tokens) > 1 {
		return "", "", fmt.Errorf("multiple URL tokens are not allowed")
	}
	tok := ""
	if len(tokens) == 1 {
		tok = tokens[0]
		if tok == "" || len(tok) > 2048 || tok != strings.TrimSpace(tok) || strings.ContainsAny(tok, "\r\n\x00") {
			return "", "", fmt.Errorf("invalid URL token")
		}
	}
	return parts[2], tok, nil
}

func mergeProfileIdentity(uid, token, urlUID, urlToken string) (ResolvedProfileInput, error) {
	if uid != "" && uid != urlUID {
		return ResolvedProfileInput{}, fmt.Errorf("user_id conflicts with profile_url")
	}
	if token != "" && urlToken != "" && token != urlToken {
		return ResolvedProfileInput{}, fmt.Errorf("xsec_token conflicts with profile_url token")
	}
	if token == "" {
		token = urlToken
	}
	canonical := "https://www.xiaohongshu.com/user/profile/" + urlUID
	return ResolvedProfileInput{UserID: urlUID, XsecToken: token, ProfileURL: canonical}, nil
}

func resolveShareURL(ctx context.Context, start *url.URL) (ResolvedProfileInput, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("invalid resolver destination")
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("share host lookup failed")
		}
		for _, candidate := range ips {
			ip := candidate.IP
			if !isPublicResolverIP(ip) {
				continue
			}
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return conn, nil
			}
		}
		return nil, fmt.Errorf("share host resolved to no public addresses")
	}}
	client := &http.Client{Timeout: 8 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return resolveShareURLWithClient(ctx, start, client)
}

func isPublicResolverIP(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified() && !ip.IsMulticast()
}

func resolveShareURLWithClient(ctx context.Context, start *url.URL, client *http.Client) (ResolvedProfileInput, error) {
	localClient := *client
	localClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client = &localClient
	current := start
	for hop := 0; hop <= profileResolveMaxRedirects; hop++ {
		if err := ctx.Err(); err != nil {
			return ResolvedProfileInput{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return ResolvedProfileInput{}, fmt.Errorf("invalid share URL")
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 13; Mobile) AppleWebKit/537.36 Chrome/120.0 Mobile Safari/537.36")
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ResolvedProfileInput{}, ctx.Err()
			}
			return ResolvedProfileInput{}, fmt.Errorf("share URL resolution failed")
		}
		if resp.Body != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, profileResolveMaxBody))
			resp.Body.Close()
		}
		if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
			if hop == profileResolveMaxRedirects {
				return ResolvedProfileInput{}, fmt.Errorf("share URL exceeded redirect limit")
			}
			loc, err := resp.Location()
			if err != nil {
				return ResolvedProfileInput{}, fmt.Errorf("share URL redirect missing location")
			}
			if !loc.IsAbs() {
				loc = current.ResolveReference(loc)
			}
			checked, err := parseSafeProfileURL(loc.String())
			if err != nil {
				return ResolvedProfileInput{}, fmt.Errorf("unsafe share URL redirect: %w", err)
			}
			if checked.Hostname() == "www.xiaohongshu.com" {
				id, tok, err := parseCanonicalProfile(checked)
				if err != nil {
					return ResolvedProfileInput{}, err
				}
				return mergeProfileIdentity("", "", id, tok)
			}
			current = checked
			continue
		}
		return ResolvedProfileInput{}, fmt.Errorf("share URL did not redirect to a profile (HTTP %d)", resp.StatusCode)
	}
	return ResolvedProfileInput{}, fmt.Errorf("share URL exceeded redirect limit")
}
