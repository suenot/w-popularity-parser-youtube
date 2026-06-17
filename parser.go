// Package parser implements the w_popularity YouTube adapter.
//
// Strategy: direct HTML scraping of the YouTube channel pages. We fetch
// `https://www.youtube.com/@<handle>/about` (for the channel snapshot) and
// `https://www.youtube.com/@<handle>/videos` (for recent uploads) with a
// modern desktop User-Agent and Accept-Language: en. Each response embeds a
// `var ytInitialData = {...};` blob which carries everything we need.
//
// We do NOT call any official YouTube Data API and we do NOT shell out to
// yt-dlp — that approach broke in late 2024 when YouTube migrated several
// channel pages to a new "pageHeaderRenderer"/"aboutChannelViewModel" shape
// in which yt-dlp's extractor returns nulls for follower / view / playlist
// counts.
//
// The parser is tolerant of both the legacy `c4TabbedHeaderRenderer` shape
// and the newer `pageHeaderRenderer` + `aboutChannelViewModel` shape. It
// walks all string leaves in ytInitialData looking for patterns such as
// "29.6M subscribers", "123,020,785,579 views", "980 videos" — so it keeps
// working as long as YouTube renders those strings somewhere in the JSON.
package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	shared "github.com/suenot/socials-auto"
)

// Config controls runtime behaviour. All fields are optional.
//
//   - APIKey is kept for backwards compatibility with callers that wired it
//     up for the old yt-dlp+API path. It is currently a no-op; the parser
//     no longer talks to the YouTube Data API.
//   - HTTPClient overrides the default client (used by tests to inject an
//     httptest.Server via a URL-rewriting RoundTripper).
//   - HTTPTimeout is the per-request budget when HTTPClient is not provided
//     (default 30s).
//   - UserAgent overrides the default desktop UA used in outgoing requests.
type Config struct {
	APIKey      string // no-op, accepted for backwards compatibility
	HTTPClient  *http.Client
	HTTPTimeout time.Duration
	UserAgent   string
}

const defaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// New returns a parser configured per cfg. It does not touch the network
// at construction time.
func New(cfg Config) *YouTubeParser {
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 30 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: cfg.HTTPTimeout}
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	return &YouTubeParser{cfg: cfg, now: time.Now}
}

// YouTubeParser is the shared.Parser implementation for YouTube.
type YouTubeParser struct {
	cfg Config
	// now is overridable in tests to make relative-date parsing deterministic.
	now func() time.Time
}

func (p *YouTubeParser) Platform() shared.Platform { return shared.PlatformYouTube }

// --- FetchChannel -----------------------------------------------------------

// FetchChannel fetches /<handle>/about and pulls Followers, PostsCount,
// TotalViews and the canonical channel id out of the embedded ytInitialData.
func (p *YouTubeParser) FetchChannel(ctx context.Context, handle string) (shared.ChannelSnapshot, error) {
	h := normalizeHandle(handle)
	channelURL := "https://www.youtube.com/@" + h
	aboutURL := channelURL + "/about"

	data, err := p.fetchInitialData(ctx, aboutURL)
	if err != nil {
		return shared.ChannelSnapshot{}, err
	}

	stats := extractChannelStats(data)

	raw := map[string]interface{}{"source": "html"}
	if stats.channelID != "" {
		raw["channel_id"] = stats.channelID
	}
	if stats.title != "" {
		raw["title"] = stats.title
	}

	return shared.ChannelSnapshot{
		Platform:   shared.PlatformYouTube,
		Handle:     h,
		URL:        channelURL,
		FetchedAt:  p.now().UTC(),
		Followers:  stats.subscribers,
		PostsCount: stats.videos,
		TotalViews: stats.views,
		Raw:        raw,
	}, nil
}

// --- FetchRecentPosts -------------------------------------------------------

// FetchRecentPosts fetches /<handle>/videos and pulls the most recent video
// entries out of the embedded ytInitialData. Posts older than `since` are
// dropped. Ordering is newest-first as returned by YouTube.
func (p *YouTubeParser) FetchRecentPosts(ctx context.Context, handle string, since time.Time) ([]shared.PostSnapshot, error) {
	h := normalizeHandle(handle)
	videosURL := "https://www.youtube.com/@" + h + "/videos"

	data, err := p.fetchInitialData(ctx, videosURL)
	if err != nil {
		return nil, err
	}

	now := p.now().UTC()
	rawVideos := extractVideos(data)
	out := make([]shared.PostSnapshot, 0, len(rawVideos))
	for _, v := range rawVideos {
		if v.videoID == "" {
			continue
		}
		published := time.Time{}
		if v.publishedAgo != "" {
			if d, ok := parseRelativeDuration(v.publishedAgo); ok {
				published = now.Add(-d)
			}
		}
		if !since.IsZero() && !published.IsZero() && published.Before(since) {
			continue
		}
		rawMeta := map[string]interface{}{"source": "html"}
		if v.title != "" {
			rawMeta["title"] = v.title
		}
		if v.publishedAgo != "" {
			rawMeta["published_ago"] = v.publishedAgo
		}
		out = append(out, shared.PostSnapshot{
			Platform:      shared.PlatformYouTube,
			ChannelHandle: h,
			PostID:        v.videoID,
			URL:           "https://www.youtube.com/watch?v=" + v.videoID,
			Kind:          shared.PostKindVideo,
			PublishedAt:   published,
			FetchedAt:     now,
			Views:         v.views,
			Raw:           rawMeta,
		})
	}
	return out, nil
}

// --- HTTP + ytInitialData extraction ----------------------------------------

// initialDataRE matches `var ytInitialData = {...};` somewhere on the page.
// YouTube terminates the assignment either with `;</script>` (modern) or
// `;var ` (older). We accept both with a non-greedy capture.
var initialDataRE = regexp.MustCompile(`(?s)var ytInitialData\s*=\s*(\{.*?\})\s*;\s*(?:</script>|var\s)`)

func (p *YouTubeParser) fetchInitialData(ctx context.Context, u string) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("youtube: %w: %v", shared.ErrTransient, err)
	}
	req.Header.Set("User-Agent", p.cfg.UserAgent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("youtube: %w", ctxErr)
		}
		return nil, fmt.Errorf("youtube: %w: %v", shared.ErrTransient, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusGone:
		return nil, fmt.Errorf("youtube: %w (http %d)", shared.ErrNotFound, resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("youtube: %w (http 429)", shared.ErrRateLimited)
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("youtube: %w (http %d)", shared.ErrAuth, resp.StatusCode)
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("youtube: %w: http %d", shared.ErrTransient, resp.StatusCode)
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("youtube: %w: http %d", shared.ErrTransient, resp.StatusCode)
	}

	// Cap the response read so a stray giant document can't blow up RAM.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("youtube: %w: read body: %v", shared.ErrTransient, err)
	}

	match := initialDataRE.FindSubmatch(body)
	if len(match) < 2 {
		return nil, fmt.Errorf("youtube: %w: YouTube response did not contain ytInitialData; UI shape may have changed", shared.ErrTransient)
	}

	var data map[string]interface{}
	if err := json.Unmarshal(match[1], &data); err != nil {
		return nil, fmt.Errorf("youtube: %w: parse ytInitialData: %v", shared.ErrTransient, err)
	}

	// YouTube serves a soft 404 with HTTP 200 + an alerts[] entry when the
	// channel doesn't exist. Detect it.
	if isChannelMissing(data) {
		return nil, fmt.Errorf("youtube: %w: channel does not exist (soft 404)", shared.ErrNotFound)
	}
	return data, nil
}

func isChannelMissing(data map[string]interface{}) bool {
	alerts, ok := data["alerts"].([]interface{})
	if !ok {
		return false
	}
	for _, a := range alerts {
		am, ok := a.(map[string]interface{})
		if !ok {
			continue
		}
		for _, v := range am {
			vm, ok := v.(map[string]interface{})
			if !ok {
				continue
			}
			t, _ := vm["type"].(string)
			if strings.EqualFold(t, "ERROR") {
				// good enough — YouTube uses this for "channel does not exist"
				return true
			}
		}
	}
	return false
}

// --- channel-stats extraction -----------------------------------------------

type channelStats struct {
	subscribers int64
	views       int64
	videos      int64
	channelID   string
	title       string
}

func extractChannelStats(data map[string]interface{}) channelStats {
	var out channelStats

	// Pass 1 — pull canonical fields from `aboutChannelViewModel` if present.
	// This is the new (post-2024) shape and contains everything as plain
	// strings: subscriberCountText, viewCountText, videoCountText, channelId.
	visit(data, func(node map[string]interface{}) {
		acvm, ok := node["aboutChannelViewModel"].(map[string]interface{})
		if !ok {
			return
		}
		if s := stringField(acvm, "subscriberCountText"); s != "" {
			if n, ok := parseAbbreviatedCount(s); ok && out.subscribers == 0 {
				out.subscribers = n
			}
		}
		if s := stringField(acvm, "viewCountText"); s != "" {
			if n, ok := parseAbbreviatedCount(s); ok && out.views == 0 {
				out.views = n
			}
		}
		if s := stringField(acvm, "videoCountText"); s != "" {
			if n, ok := parseAbbreviatedCount(s); ok && out.videos == 0 {
				out.videos = n
			}
		}
		if s := stringField(acvm, "channelId"); s != "" && out.channelID == "" {
			out.channelID = s
		}
	})

	// Pass 2 — legacy header shape `c4TabbedHeaderRenderer`.
	visit(data, func(node map[string]interface{}) {
		h, ok := node["c4TabbedHeaderRenderer"].(map[string]interface{})
		if !ok {
			return
		}
		if out.channelID == "" {
			if s := stringField(h, "channelId"); s != "" {
				out.channelID = s
			}
		}
		if out.title == "" {
			if s := stringField(h, "title"); s != "" {
				out.title = s
			}
		}
		if out.subscribers == 0 {
			if s := simpleText(h["subscriberCountText"]); s != "" {
				if n, ok := parseAbbreviatedCount(s); ok {
					out.subscribers = n
				}
			}
		}
		if out.videos == 0 {
			if s := simpleText(h["videosCountText"]); s != "" {
				if n, ok := parseAbbreviatedCount(s); ok {
					out.videos = n
				}
			}
		}
		if out.views == 0 {
			if s := simpleText(h["viewCountText"]); s != "" {
				if n, ok := parseAbbreviatedCount(s); ok {
					out.views = n
				}
			}
		}
	})

	// Pass 3 — extract metadata.channelMetadataRenderer.{title,externalId}.
	visit(data, func(node map[string]interface{}) {
		m, ok := node["channelMetadataRenderer"].(map[string]interface{})
		if !ok {
			return
		}
		if out.title == "" {
			if s := stringField(m, "title"); s != "" {
				out.title = s
			}
		}
		if out.channelID == "" {
			if s := stringField(m, "externalId"); s != "" {
				out.channelID = s
			}
		}
	})

	// Pass 4 — last resort. Walk every string leaf and try to match patterns
	// like "29.6M subscribers" or "980 videos". This catches the
	// pageHeaderRenderer.contentMetadataViewModel shape where the data sits
	// inside metadataParts[].text.content.
	if out.subscribers == 0 || out.videos == 0 || out.views == 0 {
		walkStrings(data, func(s string) {
			if out.subscribers == 0 {
				if n, ok := parseCountedString(s, subscriberWords); ok {
					out.subscribers = n
				}
			}
			if out.views == 0 {
				if n, ok := parseCountedString(s, viewsWords); ok {
					out.views = n
				}
			}
			if out.videos == 0 {
				if n, ok := parseCountedString(s, videosWords); ok {
					out.videos = n
				}
			}
		})
	}

	// Pass 5 — externalChannelId can also live in webCommandMetadata-style
	// blobs; search anywhere as a fallback.
	if out.channelID == "" {
		visit(data, func(node map[string]interface{}) {
			if out.channelID != "" {
				return
			}
			for _, k := range []string{"externalChannelId", "externalId", "channelId", "browseId"} {
				if s := stringField(node, k); s != "" && strings.HasPrefix(s, "UC") && len(s) >= 20 {
					out.channelID = s
					return
				}
			}
		})
	}
	return out
}

// --- recent-videos extraction -----------------------------------------------

type rawVideo struct {
	videoID      string
	title        string
	publishedAgo string
	views        int64
}

func extractVideos(data map[string]interface{}) []rawVideo {
	var out []rawVideo
	seen := map[string]bool{}

	// New shape: gridRenderer -> richItemRenderer -> lockupViewModel.
	visit(data, func(node map[string]interface{}) {
		lvm, ok := node["lockupViewModel"].(map[string]interface{})
		if !ok {
			return
		}
		v := rawVideo{}
		v.videoID = stringField(lvm, "contentId")
		if v.videoID == "" {
			return
		}
		if md, ok := lvm["metadata"].(map[string]interface{}); ok {
			if lmvm, ok := md["lockupMetadataViewModel"].(map[string]interface{}); ok {
				v.title = extractContent(lmvm["title"])
				if metaWrap, ok := lmvm["metadata"].(map[string]interface{}); ok {
					if cmvm, ok := metaWrap["contentMetadataViewModel"].(map[string]interface{}); ok {
						v.views, v.publishedAgo = pickViewsAndAge(cmvm)
					}
				}
			}
		}
		if !seen[v.videoID] {
			seen[v.videoID] = true
			out = append(out, v)
		}
	})

	// Older shape: videoRenderer / gridVideoRenderer.
	visit(data, func(node map[string]interface{}) {
		for _, key := range []string{"videoRenderer", "gridVideoRenderer"} {
			vr, ok := node[key].(map[string]interface{})
			if !ok {
				continue
			}
			v := rawVideo{}
			v.videoID = stringField(vr, "videoId")
			if v.videoID == "" {
				continue
			}
			v.title = extractContent(vr["title"])
			v.publishedAgo = simpleText(vr["publishedTimeText"])
			if s := simpleText(vr["viewCountText"]); s != "" {
				if n, ok := parseAbbreviatedCount(s); ok {
					v.views = n
				}
			}
			if !seen[v.videoID] {
				seen[v.videoID] = true
				out = append(out, v)
			}
		}
	})
	return out
}

// pickViewsAndAge inspects contentMetadataViewModel.metadataRows[].metadataParts[].text.content
// and returns (views, publishedAgo). Order in the rows is "X views" then
// "X ago"; we accept any order and key on the suffix.
func pickViewsAndAge(cmvm map[string]interface{}) (int64, string) {
	var views int64
	var ago string

	rows, _ := cmvm["metadataRows"].([]interface{})
	for _, r := range rows {
		rm, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		parts, _ := rm["metadataParts"].([]interface{})
		for _, p := range parts {
			pm, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			s := extractContent(pm["text"])
			if s == "" {
				continue
			}
			if n, ok := parseCountedString(s, viewsWords); ok && views == 0 {
				views = n
				continue
			}
			if isRelativeDateString(s) && ago == "" {
				ago = s
			}
		}
	}
	return views, ago
}

// --- generic walkers --------------------------------------------------------

// visit invokes fn on every map encountered in the JSON tree, including the
// root. It deliberately ignores the maximum recursion depth — ytInitialData
// is bounded by the page size we already capped at 16 MiB.
func visit(node interface{}, fn func(map[string]interface{})) {
	switch v := node.(type) {
	case map[string]interface{}:
		fn(v)
		for _, child := range v {
			visit(child, fn)
		}
	case []interface{}:
		for _, child := range v {
			visit(child, fn)
		}
	}
}

func walkStrings(node interface{}, fn func(string)) {
	switch v := node.(type) {
	case map[string]interface{}:
		for _, child := range v {
			walkStrings(child, fn)
		}
	case []interface{}:
		for _, child := range v {
			walkStrings(child, fn)
		}
	case string:
		fn(v)
	}
}

// stringField returns m[k] as a string, or "".
func stringField(m map[string]interface{}, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

// simpleText returns v.simpleText or concatenates v.runs[].text.
func simpleText(v interface{}) string {
	m, ok := v.(map[string]interface{})
	if !ok {
		return ""
	}
	if s, ok := m["simpleText"].(string); ok {
		return s
	}
	if runs, ok := m["runs"].([]interface{}); ok {
		var b strings.Builder
		for _, r := range runs {
			rm, ok := r.(map[string]interface{})
			if !ok {
				continue
			}
			if t, ok := rm["text"].(string); ok {
				b.WriteString(t)
			}
		}
		return b.String()
	}
	return ""
}

// extractContent handles the three text-bearing shapes that show up in
// YouTube's response: {"simpleText": "..."}, {"runs": [...]}, and
// {"content": "..."}.
func extractContent(v interface{}) string {
	m, ok := v.(map[string]interface{})
	if !ok {
		return ""
	}
	if s, ok := m["content"].(string); ok && s != "" {
		return s
	}
	if s, ok := m["simpleText"].(string); ok && s != "" {
		return s
	}
	if runs, ok := m["runs"].([]interface{}); ok {
		var b strings.Builder
		for _, r := range runs {
			rm, ok := r.(map[string]interface{})
			if !ok {
				continue
			}
			if t, ok := rm["text"].(string); ok {
				b.WriteString(t)
			}
		}
		return b.String()
	}
	return ""
}

// --- number parsing ---------------------------------------------------------

// Words we accept after the count, per metric. Lowercased.
var (
	subscriberWords = []string{"subscriber", "subscribers", "подписчик", "подписчика", "подписчиков"}
	viewsWords      = []string{"view", "views", "просмотр", "просмотра", "просмотров"}
	videosWords     = []string{"video", "videos", "видео"}
)

// countedStringRE matches "29.6M subscribers" / "1,234,567 views" /
// "5 миллионов просмотров"-style strings. Capture groups:
//
//	1: number with optional thousands separators & decimal
//	2: optional single-letter suffix (K, M, B) or full word ("million", "thousand", "billion")
//	3: trailing noun (subscribers, views, videos, ...)
var countedStringRE = regexp.MustCompile(`^\s*([\d.,  \s]+)\s*([KkMmBbBlnТтМмК]|thousand|million|billion|тыс|млн|млрд)?\s+([\p{L}]+)\s*$`)

// parseCountedString returns (n, true) iff s parses as "<number> <suffix?> <word>"
// where word matches one of `acceptable` (case-insensitive, prefix match —
// so "subscribers" matches "subscriber" and "видео" matches "видео").
func parseCountedString(s string, acceptable []string) (int64, bool) {
	m := countedStringRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	noun := strings.ToLower(m[3])
	ok := false
	for _, w := range acceptable {
		if noun == w {
			ok = true
			break
		}
	}
	if !ok {
		return 0, false
	}
	return parseNumberWithSuffix(m[1], m[2])
}

// parseAbbreviatedCount parses a single string of either shape:
//
//	"29.6M subscribers"        -> 29_600_000
//	"123,020,785,579 views"    -> 123020785579
//	"980 videos"               -> 980
//	"5M"                       -> 5_000_000
//	"1.2K"                     -> 1_200
//	"1,234,567"                -> 1_234_567
//
// The trailing noun (if any) is ignored — callers that care about which
// metric the string represents should use parseCountedString instead.
func parseAbbreviatedCount(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	// Try with a trailing noun first.
	if m := countedStringRE.FindStringSubmatch(s); m != nil {
		return parseNumberWithSuffix(m[1], m[2])
	}
	// Else strip and try as a bare number with optional KMB suffix.
	re := regexp.MustCompile(`^\s*([\d.,  \s]+)\s*([KkMmBb]|thousand|million|billion)?\s*$`)
	if m := re.FindStringSubmatch(s); m != nil {
		return parseNumberWithSuffix(m[1], m[2])
	}
	return 0, false
}

func parseNumberWithSuffix(numStr, suffix string) (int64, bool) {
	// Normalize: strip thousands separators (',' and various whitespace
	// no-break/narrow no-break/regular spaces). Keep '.' as decimal point.
	clean := strings.NewReplacer(
		",", "",
		" ", "",
		" ", "",
		" ", "",
	).Replace(numStr)
	if clean == "" {
		return 0, false
	}
	// If both ',' and '.' appear in the original we already stripped ',' as a
	// thousands separator. If only ',' appears AND it has no thousands-style
	// grouping, treat it as the decimal separator.
	if !strings.ContainsAny(clean, ".") && strings.Contains(numStr, ",") && !looksLikeThousands(numStr) {
		clean = strings.Replace(numStr, ",", ".", 1)
	}
	f, err := strconv.ParseFloat(clean, 64)
	if err != nil {
		return 0, false
	}
	mult := suffixMultiplier(suffix)
	val := f * mult
	if val < 0 || math.IsNaN(val) || math.IsInf(val, 0) {
		return 0, false
	}
	return int64(math.Round(val)), true
}

func looksLikeThousands(s string) bool {
	// Treat any "," followed by three digits as a thousands separator.
	for i := 0; i < len(s)-3; i++ {
		if s[i] == ',' {
			d := s[i+1 : i+4]
			if len(d) == 3 && d[0] >= '0' && d[0] <= '9' && d[1] >= '0' && d[1] <= '9' && d[2] >= '0' && d[2] <= '9' {
				return true
			}
		}
	}
	return false
}

func suffixMultiplier(suffix string) float64 {
	switch strings.ToLower(suffix) {
	case "":
		return 1
	case "k", "thousand", "тыс":
		return 1_000
	case "m", "million", "млн":
		return 1_000_000
	case "b", "billion", "bln", "млрд":
		return 1_000_000_000
	default:
		return 1
	}
}

// --- relative-date parsing --------------------------------------------------

var relativeRE = regexp.MustCompile(`^\s*(\d+)\s+([\p{L}]+)\s+([\p{L}]+)\s*$`)

// isRelativeDateString returns true iff s looks like "3 days ago" /
// "1 month ago" / "5 лет назад" / etc.
func isRelativeDateString(s string) bool {
	s = strings.TrimSpace(s)
	m := relativeRE.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	tail := strings.ToLower(m[3])
	return tail == "ago" || tail == "назад"
}

// parseRelativeDuration converts "N <unit> ago" / "N <unit> назад" to a
// time.Duration. Month and year are approximated (30d / 365d).
func parseRelativeDuration(s string) (time.Duration, bool) {
	m := relativeRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 0 {
		return 0, false
	}
	unit := strings.ToLower(strings.TrimRight(m[2], "s")) // "days" -> "day"
	d, ok := unitToDuration(unit)
	if !ok {
		return 0, false
	}
	return time.Duration(n) * d, true
}

func unitToDuration(unit string) (time.Duration, bool) {
	switch unit {
	case "second", "sec", "секунду", "секунды", "секунд":
		return time.Second, true
	case "minute", "min", "минуту", "минуты", "минут":
		return time.Minute, true
	case "hour", "hr", "час", "часа", "часов":
		return time.Hour, true
	case "day", "день", "дня", "дней":
		return 24 * time.Hour, true
	case "week", "wk", "неделю", "недели", "недель":
		return 7 * 24 * time.Hour, true
	case "month", "mo", "mon", "месяц", "месяца", "месяцев":
		return 30 * 24 * time.Hour, true
	case "year", "yr", "год", "года", "лет":
		return 365 * 24 * time.Hour, true
	}
	return 0, false
}

// --- misc -------------------------------------------------------------------

func normalizeHandle(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "@")
}

// Compile-time interface check.
var _ shared.Parser = (*YouTubeParser)(nil)
