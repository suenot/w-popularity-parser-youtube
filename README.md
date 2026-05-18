# w-popularity-parser-youtube

YouTube parser for [w_popularity](https://github.com/suenot/w-popularity).

**Status:** functional (channel + recent videos), no auth required.

## Strategy

Direct HTML scraping of `youtube.com/@<handle>/about` and
`youtube.com/@<handle>/videos` with a modern desktop User-Agent and
`Accept-Language: en`. Both pages embed a `var ytInitialData = {...}` blob
that carries everything we need.

We deliberately do **not** use:

- the official YouTube Data API v3 (would require an API key and burns quota)
- the `yt-dlp` CLI (broke in late 2024 when YouTube migrated several channel
  pages to the new `pageHeaderRenderer` / `aboutChannelViewModel` shape —
  yt-dlp's extractor returns `null` for `channel_follower_count`,
  `view_count`, `playlist_count`)

The parser is tolerant of **both** YouTube response shapes:

- new (≥2024): `aboutChannelViewModel.subscriberCountText` / `viewCountText` /
  `videoCountText`
- legacy: `header.c4TabbedHeaderRenderer.subscriberCountText` /
  `viewCountText` / `videosCountText`
- ultra-new pageHeader: walked as a last-resort string-leaf scan for patterns
  like `"487M subscribers"` / `"980 videos"`

## Usage

```go
import parser "github.com/suenot/w-popularity-parser-youtube"

p := parser.New(parser.Config{})

snap, err  := p.FetchChannel(ctx, "@MrBeast")
posts, err := p.FetchRecentPosts(ctx, "@MrBeast", time.Now().AddDate(0, 0, -7))
```

## Config

| field         | meaning                                                            | default |
|---------------|---------------------------------------------------------------------|---------|
| `APIKey`      | **no-op**, kept for backwards compatibility — ignored               | `""`    |
| `HTTPClient`  | override; used by tests to inject an httptest.Server                | shared  |
| `HTTPTimeout` | per-request budget when `HTTPClient` is not provided                | `30s`   |
| `UserAgent`   | override outgoing UA                                                | Chrome 120 desktop |

## Fields populated

`ChannelSnapshot`:

- `Followers` — from `subscriberCountText` (parses `"1.2M"`, `"487M"`,
  `"1,234,567"`, etc.)
- `PostsCount` — from `videoCountText` / `videosCountText`
- `TotalViews` — from `viewCountText`
- `Raw["channel_id"]`, `Raw["title"]`, `Raw["source"]="html"`

`PostSnapshot` (one per video on `/videos`):

- `PostID`, `URL` (`https://www.youtube.com/watch?v=<id>`), `Kind = video`
- `PublishedAt` — derived from relative strings like `"3 days ago"`,
  `"1 month ago"`, `"5 years ago"`, `"3 дня назад"`. Month/year are
  approximated (30d / 365d).
- `Views` — from `viewCountText`
- `Raw["title"]`, `Raw["published_ago"]`, `Raw["source"]="html"`
- `Likes` and `Comments` are not exposed on the channel listing page, so they
  remain zero. (Per-video pages have them but each one costs an extra
  request; not worth it for a batch sweep.)

## Error mapping

| HTTP                                | mapped to                |
|-------------------------------------|--------------------------|
| 404, 410                            | `shared.ErrNotFound`     |
| HTTP 200 + alerts[].type=`"ERROR"`  | `shared.ErrNotFound` (soft 404) |
| 429                                 | `shared.ErrRateLimited`  |
| 401, 403                            | `shared.ErrAuth`         |
| 5xx, network errors, parse failures | `shared.ErrTransient`    |
| Body without `ytInitialData`        | `shared.ErrTransient`    |

## Rate-limit notes

Anonymous web scraping has no quota but YouTube *will* eventually 429 if you
hammer the same handle. Cache results and stagger requests.

## License

MIT
