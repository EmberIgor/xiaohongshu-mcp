# Bounded search scrolling (custom fork)

The `custom` branch carries the bounded-scroll patch on upstream v2.5.5. Keep
`main` aligned with upstream; review and test upstream merges before deployment.
The upstream license is unchanged.

`search_feeds` accepts `max_scrolls` (integer 0–3, default 0) and optional `limit`
(integer 1–200). HTTP GET/POST `/api/v1/feeds/search` accepts the same options.
Use `max_scrolls: 3` for ordinary bounded collection; omit it for the original
initial-page behavior. A limit can stop collection before three rounds.

The response retains `feeds` and `count`, adding `initial_count`,
`scrolls_performed`, and `stop_reason`. Notes are deduplicated by ID, retaining
the first complete note. Returned counts depend on actual platform results;
three scrolls do not guarantee a fixed count. Two rounds without new notes stop
early. Login loss, verification challenges, cancellation and browser errors
stop collection; partial results may report the reason in metadata.

The passive fetch/XHR capture is bounded and avoids replacing its shared queue.
No direct signed search API or unlimited scrolling is introduced.

Verification:

```sh
go test ./... -count=1
go vet ./...
XHS_TEST_BROWSER=/path/to/standalone/chromium go test ./xiaohongshu -run TestSearchBrowserFixture -count=1
```

The browser fixture is opt-in and uses local synthetic data, not an account.
Keep real-account acceptance responses, tokens, cookies and logs private.

Build the identifiable custom version with:

```sh
go build -ldflags '-X main.version=v2.5.5-igor.1' -o bin/xiaohongshu-mcp .
```

For upgrades, retain the prior binary and a restricted local cookie backup,
atomically replace the binary, restart the existing supervisor, and verify the
actual health version, tool schema and one explicitly authorized bounded search.
Rollback restores the prior binary atomically and restarts the same supervisor;
do not overwrite cookies unless a separately diagnosed issue requires it.
