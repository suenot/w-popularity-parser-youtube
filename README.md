# w-popularity-parser-youtube

YouTube parser for [w_popularity](https://github.com/suenot/w-popularity).

**Status:** functional (channel + recent videos).

## Strategy

- **Primary:** YouTube Data API v3 (requires an API key — get one at https://console.cloud.google.com/apis/credentials)
- **Fallback:** yt-dlp (not yet implemented)

API endpoints used:
- `channels?forHandle=@h&part=statistics,snippet` → subscriber/view/video count
- `search?channelId=&type=video&order=date&publishedAfter=` → recent video IDs
- `videos?id=…&part=statistics,snippet` → per-video like/view/comment counts

## Usage

```go
import parser "github.com/suenot/w-popularity-parser-youtube"

p := parser.New(parser.Config{APIKey: os.Getenv("YOUTUBE_API_KEY")})
snap, err := p.FetchChannel(ctx, "@marketmaker-cc")
posts, err := p.FetchRecentPosts(ctx, "@marketmaker-cc", time.Now().AddDate(0, 0, -7))
```

## Quota

Each `FetchChannel` call costs 1 quota unit. Each `FetchRecentPosts` call costs ~101 units
(1 for the channels lookup, 100 for the search). Default project quota is 10,000 units/day —
plenty for daily snapshots of ~100 channels.

## License

MIT
