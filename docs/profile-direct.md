# Direct user profile input

`user_profile` keeps its existing tool name, `user_id`, `xsec_token`, `tab`, and profile response fields. It now accepts a bare user ID without a token, a canonical profile URL, or a supported share URL through optional `profile_url`.

## Input contract

At least one of `user_id` or `profile_url` is required. A user ID is exactly 24 lowercase hexadecimal characters, preserved exactly; uppercase is rejected rather than normalized. Surrounding whitespace and other characters are rejected. Supplying both forms is allowed only when their IDs match. `xsec_token` is optional, at most 2048 characters, and may not contain surrounding whitespace or control characters.

Canonical URLs must use HTTPS and the exact host `www.xiaohongshu.com`, with path `/user/profile/<user_id>`. Accepted query keys are `xsec_token`, `xsec_source`, and the common app-share tracking keys `share_from_user_hidden`, `xhsshare`, `share_id`, `source`, `app_platform`, `apptime`, and `shareRedId`; keys may appear once and values are bounded. `tab` is selected through the existing `tab` field. A URL token is used only for that URL. If both token fields are nonempty, they must match. No token is synthesized or taken from another request. A share URL's query token is not borrowed; only a token on the final canonical profile URL is extracted.

Share URLs are accepted only on exact hosts `xhslink.cn` and `xhslink.com`. Resolution sends a bounded mobile user-agent GET (8 seconds per request, 20 seconds overall, 64 KiB response-body cap), follows at most five redirects, and validates HTTPS, host, port, credentials, and URL shape at every hop. Only public DNS addresses are dialed. The final redirect must be a canonical profile URL. Arbitrary schemes, ports, credentials, fragments, note URLs, and other hosts fail before browser startup. HEAD is not used.

Examples use synthetic identifiers and tokens:

```json
{"user_id":"0123456789abcdef01234567"}
```

```json
{"profile_url":"https://www.xiaohongshu.com/user/profile/0123456789abcdef01234567?xsec_token=synthetic-token"}
```

```json
{"profile_url":"https://xhslink.com/synthetic-share"}
```

```json
{"user_id":"0123456789abcdef01234567","xsec_token":"synthetic-token","profile_url":"https://www.xiaohongshu.com/user/profile/0123456789abcdef01234567"}
```

## Result and failure behavior

The existing `userBasicInfo`, `interactions`, and `feeds` fields remain. For direct `user_profile`, the additive `user_id` field comes from structured profile state (`basicInfo.userId`, a profile user-state UID, or one consistent UID from the profile's own note authors), must match the requested ID, and is checked against all available structured note-author IDs for the note tab. Favorites and liked-note authors are not profile identity evidence. URL path alone is not treated as proof of account identity. A direct profile page without a reliable structured UID or useful structured profile data is an error. `get_my_profile` retains its existing sidebar behavior and response shape; it may return useful structured profile data when that legacy page state omits a UID. Login gates, captcha/security verification, risk-control text, unavailable pages, navigation failures, cancellation, and identity mismatch return errors; requests are not retried or switched to another account/backend. The browser and page are closed on every service return path.

The optional `tab` behavior remains `note` by default, with existing `fav` and `liked` options. Existing `user_id` + `xsec_token` calls continue to navigate the canonical profile with their supplied token.
