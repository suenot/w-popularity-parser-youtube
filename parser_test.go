package parser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	shared "github.com/suenot/w-popularity-shared"
)

// --- test plumbing ----------------------------------------------------------

// newWithServer wires the parser to send every youtube.com request to the
// provided httptest.Server. The Path/Query are preserved so the handler can
// route on `/@<handle>/about` etc.
func newWithServer(t *testing.T, srv *httptest.Server) *YouTubeParser {
	t.Helper()
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Transport: rewriteRT{target: target, next: http.DefaultTransport},
		Timeout:   5 * time.Second,
	}
	p := New(Config{HTTPClient: client})
	// Pin "now" so relative-date math is deterministic in tests.
	p.now = func() time.Time {
		return time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	}
	return p
}

type rewriteRT struct {
	target *url.URL
	next   http.RoundTripper
}

func (r rewriteRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Host, "youtube.com") {
		req2 := req.Clone(req.Context())
		req2.URL.Scheme = r.target.Scheme
		req2.URL.Host = r.target.Host
		req2.Host = r.target.Host
		return r.next.RoundTrip(req2)
	}
	return r.next.RoundTrip(req)
}

// htmlWithInitialData wraps the given ytInitialData JSON in a minimal HTML
// document shaped like a real YouTube page.
func htmlWithInitialData(json string) string {
	return `<!DOCTYPE html><html><head><title>YouTube</title></head><body>` +
		`<script nonce="x">var ytInitialData = ` + json + `;</script>` +
		`</body></html>`
}

// --- FetchChannel: new aboutChannelViewModel shape --------------------------

func TestFetchChannel_AboutChannelViewModel(t *testing.T) {
	body := htmlWithInitialData(`{
		"metadata": {
			"channelMetadataRenderer": {
				"title": "MarketMaker",
				"externalId": "UCabc123channel"
			}
		},
		"contents": {
			"twoColumnBrowseResultsRenderer": {
				"tabs": [{"tabRenderer": {
					"content": {"sectionListRenderer": {"contents": [{
						"itemSectionRenderer": {"contents": [{
							"channelAboutFullMetadataRenderer": {
								"aboutChannelViewModel": {
									"subscriberCountText": "1.2M subscribers",
									"viewCountText": "15,000,000 views",
									"videoCountText": "234 videos",
									"channelId": "UCabc123channel"
								}
							}
						}]}
					}]}}
				}}]
			}
		}
	}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/about") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := newWithServer(t, srv)
	snap, err := p.FetchChannel(context.Background(), "@mm")
	if err != nil {
		t.Fatalf("FetchChannel: %v", err)
	}
	if snap.Platform != shared.PlatformYouTube {
		t.Fatalf("platform: %s", snap.Platform)
	}
	if snap.Handle != "mm" {
		t.Fatalf("handle: %s", snap.Handle)
	}
	if snap.Followers != 1_200_000 {
		t.Fatalf("followers: got %d, want 1_200_000", snap.Followers)
	}
	if snap.TotalViews != 15_000_000 {
		t.Fatalf("total_views: got %d, want 15_000_000", snap.TotalViews)
	}
	if snap.PostsCount != 234 {
		t.Fatalf("posts_count: got %d, want 234", snap.PostsCount)
	}
	if id, _ := snap.Raw["channel_id"].(string); id != "UCabc123channel" {
		t.Fatalf("channel_id: %v", snap.Raw["channel_id"])
	}
	if src, _ := snap.Raw["source"].(string); src != "html" {
		t.Fatalf("source: %v", snap.Raw["source"])
	}
	if snap.URL != "https://www.youtube.com/@mm" {
		t.Fatalf("url: %s", snap.URL)
	}
}

// --- FetchChannel: legacy c4TabbedHeaderRenderer shape ----------------------

func TestFetchChannel_LegacyC4TabbedHeader(t *testing.T) {
	body := htmlWithInitialData(`{
		"header": {
			"c4TabbedHeaderRenderer": {
				"channelId": "UCold",
				"title": "Legacy Channel",
				"subscriberCountText": {"simpleText": "123K subscribers"},
				"videosCountText": {"runs": [{"text": "50"}, {"text": " videos"}]}
			}
		},
		"metadata": {
			"channelMetadataRenderer": {
				"externalId": "UCold",
				"title": "Legacy Channel"
			}
		},
		"contents": {
			"twoColumnBrowseResultsRenderer": {
				"tabs": [{"tabRenderer": {
					"content": {"sectionListRenderer": {"contents": [{
						"itemSectionRenderer": {"contents": [{
							"channelAboutFullMetadataRenderer": {
								"viewCountText": {"simpleText": "1,234,567 views"}
							}
						}]}
					}]}}
				}}]
			}
		}
	}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := newWithServer(t, srv)
	snap, err := p.FetchChannel(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("FetchChannel: %v", err)
	}
	if snap.Followers != 123_000 {
		t.Fatalf("followers: got %d, want 123_000", snap.Followers)
	}
	if snap.PostsCount != 50 {
		t.Fatalf("posts_count: got %d, want 50", snap.PostsCount)
	}
	if snap.TotalViews != 1_234_567 {
		t.Fatalf("views: got %d, want 1_234_567", snap.TotalViews)
	}
	if id, _ := snap.Raw["channel_id"].(string); id != "UCold" {
		t.Fatalf("channel_id: %v", snap.Raw["channel_id"])
	}
}

// --- FetchChannel: newer pageHeaderRenderer fallback walk -------------------

func TestFetchChannel_PageHeaderRendererFallback(t *testing.T) {
	// In this shape there is no aboutChannelViewModel and no
	// c4TabbedHeaderRenderer — counts live inside pageHeaderViewModel's
	// metadataRows[].metadataParts[].text.content as plain strings.
	body := htmlWithInitialData(`{
		"header": {
			"pageHeaderRenderer": {
				"pageTitle": "PageHeader Channel",
				"content": {
					"pageHeaderViewModel": {
						"metadata": {
							"contentMetadataViewModel": {
								"metadataRows": [
									{"metadataParts": [
										{"text": {"content": "@phc"}},
										{"text": {"content": "487M subscribers"}},
										{"text": {"content": "980 videos"}}
									]}
								]
							}
						}
					}
				}
			}
		},
		"metadata": {
			"channelMetadataRenderer": {
				"externalId": "UCxxxxxxxxxxxxxxxxxxxxxx",
				"title": "PageHeader Channel"
			}
		}
	}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := newWithServer(t, srv)
	snap, err := p.FetchChannel(context.Background(), "phc")
	if err != nil {
		t.Fatalf("FetchChannel: %v", err)
	}
	if snap.Followers != 487_000_000 {
		t.Fatalf("followers: got %d, want 487_000_000", snap.Followers)
	}
	if snap.PostsCount != 980 {
		t.Fatalf("posts_count: got %d, want 980", snap.PostsCount)
	}
	if id, _ := snap.Raw["channel_id"].(string); id != "UCxxxxxxxxxxxxxxxxxxxxxx" {
		t.Fatalf("channel_id: %v", snap.Raw["channel_id"])
	}
}

// --- FetchRecentPosts: lockupViewModel shape, since-filter ------------------

func TestFetchRecentPosts_LockupViewModel(t *testing.T) {
	// 3 videos: 2 days ago (fresh), 3 weeks ago (fresh-ish), 2 years ago (old).
	// `since` = 30 days back from fixed `now`; only the first two survive.
	body := htmlWithInitialData(`{
		"contents": {"twoColumnBrowseResultsRenderer": {"tabs": [{"tabRenderer": {
			"content": {"richGridRenderer": {"contents": [
				{"richItemRenderer": {"content": {"lockupViewModel": {
					"contentId": "vidFRESH",
					"metadata": {"lockupMetadataViewModel": {
						"title": {"content": "Fresh Upload"},
						"metadata": {"contentMetadataViewModel": {"metadataRows": [
							{"metadataParts": [
								{"text": {"content": "43M views"}},
								{"text": {"content": "2 days ago"}}
							]}
						]}}
					}}
				}}}},
				{"richItemRenderer": {"content": {"lockupViewModel": {
					"contentId": "vidMID",
					"metadata": {"lockupMetadataViewModel": {
						"title": {"content": "Three Weeks Old"},
						"metadata": {"contentMetadataViewModel": {"metadataRows": [
							{"metadataParts": [
								{"text": {"content": "1.5K views"}},
								{"text": {"content": "3 weeks ago"}}
							]}
						]}}
					}}
				}}}},
				{"richItemRenderer": {"content": {"lockupViewModel": {
					"contentId": "vidOLD",
					"metadata": {"lockupMetadataViewModel": {
						"title": {"content": "Ancient"},
						"metadata": {"contentMetadataViewModel": {"metadataRows": [
							{"metadataParts": [
								{"text": {"content": "999 views"}},
								{"text": {"content": "2 years ago"}}
							]}
						]}}
					}}
				}}}}
			]}}
		}}]}}
	}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/videos") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := newWithServer(t, srv)
	// `now` is 2026-05-18, so since = 2026-04-13 keeps "2 days" and
	// "3 weeks" (~21d ago = 2026-04-27) but drops "2 years".
	since := time.Date(2026, 4, 13, 0, 0, 0, 0, time.UTC)
	posts, err := p.FetchRecentPosts(context.Background(), "@mb", since)
	if err != nil {
		t.Fatalf("FetchRecentPosts: %v", err)
	}
	if len(posts) != 2 {
		t.Fatalf("want 2 posts after since filter, got %d: %+v", len(posts), posts)
	}

	want := map[string]struct {
		views int64
		title string
	}{
		"vidFRESH": {43_000_000, "Fresh Upload"},
		"vidMID":   {1_500, "Three Weeks Old"},
	}
	for _, got := range posts {
		w, ok := want[got.PostID]
		if !ok {
			t.Fatalf("unexpected post id: %s", got.PostID)
		}
		if got.Views != w.views {
			t.Fatalf("views for %s: got %d want %d", got.PostID, got.Views, w.views)
		}
		if title, _ := got.Raw["title"].(string); title != w.title {
			t.Fatalf("title for %s: got %v want %s", got.PostID, got.Raw["title"], w.title)
		}
		if got.URL != "https://www.youtube.com/watch?v="+got.PostID {
			t.Fatalf("url: %s", got.URL)
		}
		if got.Kind != shared.PostKindVideo {
			t.Fatalf("kind: %s", got.Kind)
		}
		if got.PublishedAt.IsZero() {
			t.Fatalf("published_at zero for %s", got.PostID)
		}
		if got.ChannelHandle != "mb" {
			t.Fatalf("channel_handle: %s", got.ChannelHandle)
		}
	}
}

// --- FetchRecentPosts: older videoRenderer shape ----------------------------

func TestFetchRecentPosts_VideoRendererShape(t *testing.T) {
	body := htmlWithInitialData(`{
		"contents": {"twoColumnBrowseResultsRenderer": {"tabs": [{"tabRenderer": {
			"content": {"sectionListRenderer": {"contents": [{
				"itemSectionRenderer": {"contents": [{
					"gridRenderer": {"items": [
						{"gridVideoRenderer": {
							"videoId": "old1",
							"title": {"runs": [{"text": "Old Style Video"}]},
							"publishedTimeText": {"simpleText": "1 week ago"},
							"viewCountText": {"simpleText": "12,345 views"}
						}}
					]}
				}]}
			}]}}
		}}]}}
	}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := newWithServer(t, srv)
	posts, err := p.FetchRecentPosts(context.Background(), "@vr", time.Time{})
	if err != nil {
		t.Fatalf("FetchRecentPosts: %v", err)
	}
	if len(posts) != 1 {
		t.Fatalf("want 1 post, got %d", len(posts))
	}
	got := posts[0]
	if got.PostID != "old1" || got.Views != 12_345 {
		t.Fatalf("post: %+v", got)
	}
}

// --- error cases ------------------------------------------------------------

func TestFetchChannel_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := newWithServer(t, srv)
	_, err := p.FetchChannel(context.Background(), "@nope")
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestFetchChannel_429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := newWithServer(t, srv)
	_, err := p.FetchChannel(context.Background(), "@mm")
	if !errors.Is(err, shared.ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
}

func TestFetchChannel_NoYtInitialData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>no script here</body></html>`))
	}))
	defer srv.Close()

	p := newWithServer(t, srv)
	_, err := p.FetchChannel(context.Background(), "@mm")
	if !errors.Is(err, shared.ErrTransient) {
		t.Fatalf("want ErrTransient, got %v", err)
	}
	if !strings.Contains(err.Error(), "did not contain ytInitialData") {
		t.Fatalf("error message: %v", err)
	}
}

func TestFetchChannel_SoftNotFound(t *testing.T) {
	// HTTP 200 but with a YouTube alerts[]:{alertRenderer:{type:"ERROR"}} blob.
	body := htmlWithInitialData(`{
		"alerts": [{"alertRenderer": {"type": "ERROR", "text": {"simpleText": "This channel does not exist."}}}]
	}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := newWithServer(t, srv)
	_, err := p.FetchChannel(context.Background(), "@ghost")
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// --- Platform sanity check --------------------------------------------------

func TestPlatform(t *testing.T) {
	p := New(Config{})
	if p.Platform() != shared.PlatformYouTube {
		t.Fatalf("platform: %s", p.Platform())
	}
}

// --- unit tests for the numeric helpers -------------------------------------

func TestParseAbbreviatedCount(t *testing.T) {
	cases := []struct {
		in     string
		want   int64
		wantOK bool
	}{
		{"1.2K", 1_200, true},
		{"5M", 5_000_000, true},
		{"3.4B", 3_400_000_000, true},
		{"1,234,567", 1_234_567, true},
		{"487M subscribers", 487_000_000, true},
		{"980 videos", 980, true},
		{"123,020,785,579 views", 123_020_785_579, true},
		{"  ", 0, false},
		{"nonsense", 0, false},
	}
	for _, c := range cases {
		got, ok := parseAbbreviatedCount(c.in)
		if ok != c.wantOK {
			t.Errorf("parseAbbreviatedCount(%q): ok=%v want %v", c.in, ok, c.wantOK)
			continue
		}
		if got != c.want {
			t.Errorf("parseAbbreviatedCount(%q): got %d want %d", c.in, got, c.want)
		}
	}
}

func TestParseRelativeDuration(t *testing.T) {
	cases := []struct {
		in     string
		want   time.Duration
		wantOK bool
	}{
		{"3 days ago", 3 * 24 * time.Hour, true},
		{"2 weeks ago", 14 * 24 * time.Hour, true},
		{"1 month ago", 30 * 24 * time.Hour, true},
		{"5 years ago", 5 * 365 * 24 * time.Hour, true},
		{"3 дня назад", 3 * 24 * time.Hour, true},
		{"not a date", 0, false},
	}
	for _, c := range cases {
		got, ok := parseRelativeDuration(c.in)
		if ok != c.wantOK {
			t.Errorf("parseRelativeDuration(%q): ok=%v want %v", c.in, ok, c.wantOK)
			continue
		}
		if got != c.want {
			t.Errorf("parseRelativeDuration(%q): got %v want %v", c.in, got, c.want)
		}
	}
}
