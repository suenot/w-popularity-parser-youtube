# w-popularity-parser-youtube

YouTube parser for [w_popularity](https://github.com/suenot/w-popularity).

**Status:** functional (channel + recent videos).

## Strategy

- **Primary: yt-dlp** (no API key required). The parser shells out to the `yt-dlp` CLI with `-J --skip-download` against the channel's `/about` and `/videos` pages and reads the embedded JSON.
- **Fallback: YouTube Data API v3** — used only when `Config.APIKey` is set, e.g. when yt-dlp is unavailable or returns a transient error.

If neither `yt-dlp` is on `PATH` (or `Config.YTDLPPath`) **and** `Config.APIKey` is empty, the parser returns `shared.ErrAuth`.

## Requirements

- `yt-dlp` available on `PATH`. Install via:
  - macOS: `brew install yt-dlp`
  - Linux / CI: `pip install --user yt-dlp` (works on `python:slim` Docker images)
  - Or pin a path via `Config.YTDLPPath`

## Usage

```go
import parser "github.com/suenot/w-popularity-parser-youtube"

// yt-dlp on PATH; no API key needed.
p := parser.New(parser.Config{})

snap, err := p.FetchChannel(ctx, "@marketmaker-cc")
posts, err := p.FetchRecentPosts(ctx, "@marketmaker-cc", time.Now().AddDate(0, 0, -7))
```

With an explicit binary path and an optional API fallback:

```go
p := parser.New(parser.Config{
    YTDLPPath: "/opt/homebrew/bin/yt-dlp",
    APIKey:    os.Getenv("YOUTUBE_API_KEY"), // optional fallback
    HTTPTimeout: 90 * time.Second,
})
```

## Fields populated

- `ChannelSnapshot`: `Followers` (`channel_follower_count`), `PostsCount` (`playlist_count`), `TotalViews` (`view_count`). `Raw` includes `channel_id`, `title`, `source` (`"yt-dlp"` or `"api"`).
- `PostSnapshot`: `PostID`, `URL`, `PublishedAt`, `Views`, `Likes`, `Comments`. `Kind = video`. Filtered by `since`.

## Error mapping

yt-dlp exit-code != 0, parsed from stderr:

| stderr contains | mapped to |
| --- | --- |
| `private video`, `members-only`, `does not exist`, `not found` | `shared.ErrNotFound` |
| `rate-limited`, `too many requests`, `http error 4xx` | `shared.ErrRateLimited` |
| anything else | `shared.ErrTransient` |
| ctx cancelled / deadline | wrapped `ctx.Err()` |

## Quota

yt-dlp uses anonymous web requests, so there's no API quota — but be polite: cache results and don't hammer the same handle.

## License

MIT
