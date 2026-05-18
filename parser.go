// Package parser implements the w_popularity YouTube adapter.
//
// Strategy:
//
//	primary:  yt-dlp CLI (no API key required).
//	fallback: YouTube Data API v3 (only when Config.APIKey is set).
//
// yt-dlp is invoked with `-J --skip-download` against
// `https://www.youtube.com/@<handle>/about` (for channel snapshots) and
// `https://www.youtube.com/@<handle>/videos` (for the 50 most recent videos).
// Its JSON output is parsed into shared.ChannelSnapshot / shared.PostSnapshot.
package parser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	shared "github.com/suenot/w-popularity-shared"
)

const apiBase = "https://www.googleapis.com/youtube/v3"

// Config controls runtime behaviour. Either field may be empty.
//
//   - YTDLPPath: explicit path to a yt-dlp executable. When empty, exec.LookPath
//     is used. When that fails too, FetchChannel returns shared.ErrAuth.
//   - APIKey: optional fallback to YouTube Data API v3.
//   - HTTPTimeout: overall budget for an individual yt-dlp invocation
//     (default 60s) or an API HTTP call (default 15s).
type Config struct {
	YTDLPPath   string
	APIKey      string
	HTTPClient  *http.Client
	HTTPTimeout time.Duration
}

// New returns a parser configured per cfg. It does not touch the network or
// the filesystem at construction time.
func New(cfg Config) *YouTubeParser {
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 60 * time.Second
	}
	if cfg.HTTPClient == nil {
		// API fallback uses a shorter budget.
		apiTimeout := 15 * time.Second
		if cfg.HTTPTimeout < apiTimeout {
			apiTimeout = cfg.HTTPTimeout
		}
		cfg.HTTPClient = &http.Client{Timeout: apiTimeout}
	}
	return &YouTubeParser{cfg: cfg}
}

type YouTubeParser struct{ cfg Config }

func (p *YouTubeParser) Platform() shared.Platform { return shared.PlatformYouTube }

// --- yt-dlp JSON shape (subset). ---------------------------------------------

// ytdlpEntry covers both the top-level "About"/"Videos" page metadata and the
// per-video entries returned by --playlist-end > 0. All fields are optional
// because yt-dlp emits sparse output depending on the URL.
type ytdlpEntry struct {
	ID                   string       `json:"id"`
	Title                string       `json:"title"`
	WebpageURL           string       `json:"webpage_url"`
	UploadDate           string       `json:"upload_date"` // YYYYMMDD UTC
	Timestamp            int64        `json:"timestamp"`   // unix seconds (when present)
	ReleaseTimestamp     int64        `json:"release_timestamp"`
	ViewCount            int64        `json:"view_count"`
	LikeCount            int64        `json:"like_count"`
	CommentCount         int64        `json:"comment_count"`
	ChannelID            string       `json:"channel_id"`
	ChannelFollowerCount int64        `json:"channel_follower_count"`
	PlaylistCount        int64        `json:"playlist_count"`
	Entries              []ytdlpEntry `json:"entries,omitempty"`
}

// --- Channel ----------------------------------------------------------------

func (p *YouTubeParser) FetchChannel(ctx context.Context, handle string) (shared.ChannelSnapshot, error) {
	h := strings.TrimPrefix(handle, "@")
	channelURL := "https://www.youtube.com/@" + h

	// Try yt-dlp first.
	bin, ok := p.resolveYTDLP()
	if ok {
		snap, err := p.fetchChannelYTDLP(ctx, bin, h)
		if err == nil {
			return snap, nil
		}
		// If yt-dlp gave a definitive answer (not-found, rate-limited), return it.
		if errors.Is(err, shared.ErrNotFound) || errors.Is(err, shared.ErrRateLimited) {
			return shared.ChannelSnapshot{}, err
		}
		// Otherwise, fall through to the API if we have a key.
		if p.cfg.APIKey == "" {
			return shared.ChannelSnapshot{}, err
		}
	} else if p.cfg.APIKey == "" {
		return shared.ChannelSnapshot{},
			fmt.Errorf("youtube: %w: yt-dlp not installed and no APIKey configured", shared.ErrAuth)
	}

	// API fallback.
	snap, err := p.fetchChannelAPI(ctx, h)
	if err != nil {
		return shared.ChannelSnapshot{}, err
	}
	if snap.URL == "" {
		snap.URL = channelURL
	}
	return snap, nil
}

func (p *YouTubeParser) fetchChannelYTDLP(ctx context.Context, bin, handle string) (shared.ChannelSnapshot, error) {
	// /about page is light and usually carries channel_follower_count + view_count.
	// --playlist-end 0 prevents yt-dlp from descending into every video.
	args := []string{
		"-J",
		"--skip-download",
		"--no-warnings",
		"--no-call-home",
		"--ignore-config",
		"--playlist-end", "0",
		"https://www.youtube.com/@" + handle + "/about",
	}
	entry, err := p.runYTDLP(ctx, bin, args)
	if err != nil {
		return shared.ChannelSnapshot{}, err
	}

	// If /about returned nothing usable, retry against /videos which always
	// carries channel_follower_count too.
	if entry.ChannelFollowerCount == 0 && entry.ViewCount == 0 && entry.PlaylistCount == 0 {
		args2 := []string{
			"-J",
			"--skip-download",
			"--no-warnings",
			"--no-call-home",
			"--ignore-config",
			"--playlist-end", "0",
			"https://www.youtube.com/@" + handle + "/videos",
		}
		e2, err := p.runYTDLP(ctx, bin, args2)
		if err != nil {
			return shared.ChannelSnapshot{}, err
		}
		entry = e2
	}

	raw := map[string]interface{}{}
	if entry.ChannelID != "" {
		raw["channel_id"] = entry.ChannelID
	}
	if entry.Title != "" {
		raw["title"] = entry.Title
	}
	raw["source"] = "yt-dlp"

	return shared.ChannelSnapshot{
		Platform:   shared.PlatformYouTube,
		Handle:     handle,
		URL:        "https://www.youtube.com/@" + handle,
		FetchedAt:  time.Now().UTC(),
		Followers:  entry.ChannelFollowerCount,
		PostsCount: entry.PlaylistCount,
		TotalViews: entry.ViewCount,
		Raw:        raw,
	}, nil
}

// --- Posts ------------------------------------------------------------------

func (p *YouTubeParser) FetchRecentPosts(ctx context.Context, handle string, since time.Time) ([]shared.PostSnapshot, error) {
	h := strings.TrimPrefix(handle, "@")

	bin, ok := p.resolveYTDLP()
	if ok {
		posts, err := p.fetchRecentPostsYTDLP(ctx, bin, h, since)
		if err == nil {
			return posts, nil
		}
		if errors.Is(err, shared.ErrNotFound) || errors.Is(err, shared.ErrRateLimited) {
			return nil, err
		}
		if p.cfg.APIKey == "" {
			return nil, err
		}
	} else if p.cfg.APIKey == "" {
		return nil, fmt.Errorf("youtube: %w: yt-dlp not installed and no APIKey configured", shared.ErrAuth)
	}

	return p.fetchRecentPostsAPI(ctx, h, since)
}

func (p *YouTubeParser) fetchRecentPostsYTDLP(ctx context.Context, bin, handle string, since time.Time) ([]shared.PostSnapshot, error) {
	args := []string{
		"-J",
		"--skip-download",
		"--no-warnings",
		"--no-call-home",
		"--ignore-config",
		"--no-progress",
		"--playlist-end", "50",
		"https://www.youtube.com/@" + handle + "/videos",
	}
	root, err := p.runYTDLP(ctx, bin, args)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	out := make([]shared.PostSnapshot, 0, len(root.Entries))
	for _, e := range root.Entries {
		if e.ID == "" {
			continue
		}
		published := parseUploadDate(e)
		if !since.IsZero() && !published.IsZero() && published.Before(since) {
			continue
		}
		url := e.WebpageURL
		if url == "" {
			url = "https://www.youtube.com/watch?v=" + e.ID
		}
		raw := map[string]interface{}{"source": "yt-dlp"}
		if e.Title != "" {
			raw["title"] = e.Title
		}
		out = append(out, shared.PostSnapshot{
			Platform:      shared.PlatformYouTube,
			ChannelHandle: handle,
			PostID:        e.ID,
			URL:           url,
			Kind:          shared.PostKindVideo,
			PublishedAt:   published,
			FetchedAt:     now,
			Likes:         e.LikeCount,
			Views:         e.ViewCount,
			Comments:      e.CommentCount,
			Raw:           raw,
		})
	}
	return out, nil
}

func parseUploadDate(e ytdlpEntry) time.Time {
	if e.Timestamp > 0 {
		return time.Unix(e.Timestamp, 0).UTC()
	}
	if e.ReleaseTimestamp > 0 {
		return time.Unix(e.ReleaseTimestamp, 0).UTC()
	}
	if e.UploadDate != "" {
		t, err := time.Parse("20060102", e.UploadDate)
		if err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// --- yt-dlp invocation ------------------------------------------------------

func (p *YouTubeParser) resolveYTDLP() (string, bool) {
	if p.cfg.YTDLPPath != "" {
		return p.cfg.YTDLPPath, true
	}
	bin, err := exec.LookPath("yt-dlp")
	if err != nil {
		return "", false
	}
	return bin, true
}

func (p *YouTubeParser) runYTDLP(ctx context.Context, bin string, args []string) (ytdlpEntry, error) {
	// Enforce a budget on top of any ctx deadline already set by the caller.
	if p.cfg.HTTPTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.cfg.HTTPTimeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ytdlpEntry{}, fmt.Errorf("youtube yt-dlp: %w", ctxErr)
		}
		return ytdlpEntry{}, mapYTDLPError(err, stderr.String())
	}

	var entry ytdlpEntry
	if err := json.Unmarshal(stdout.Bytes(), &entry); err != nil {
		return ytdlpEntry{}, fmt.Errorf("youtube yt-dlp: %w: parse json: %v", shared.ErrTransient, err)
	}
	return entry, nil
}

func mapYTDLPError(runErr error, stderr string) error {
	low := strings.ToLower(stderr)
	switch {
	case strings.Contains(low, "private video"),
		strings.Contains(low, "members-only"),
		strings.Contains(low, "this channel does not exist"),
		strings.Contains(low, "does not exist"),
		strings.Contains(low, "not found"),
		strings.Contains(low, "unable to find"):
		return fmt.Errorf("youtube yt-dlp: %w: %s", shared.ErrNotFound, trimErr(stderr))
	case strings.Contains(low, "rate-limited"),
		strings.Contains(low, "too many requests"),
		strings.Contains(low, "http error 429"),
		strings.Contains(low, "http error 4"):
		return fmt.Errorf("youtube yt-dlp: %w: %s", shared.ErrRateLimited, trimErr(stderr))
	default:
		return fmt.Errorf("youtube yt-dlp: %w: %v: %s", shared.ErrTransient, runErr, trimErr(stderr))
	}
}

func trimErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 256 {
		s = s[:256] + "..."
	}
	return s
}

// --- YouTube Data API v3 fallback ------------------------------------------

type channelsResp struct {
	Items []struct {
		ID      string `json:"id"`
		Snippet struct {
			Title       string    `json:"title"`
			CustomURL   string    `json:"customUrl"`
			PublishedAt time.Time `json:"publishedAt"`
		} `json:"snippet"`
		Statistics struct {
			ViewCount       string `json:"viewCount"`
			SubscriberCount string `json:"subscriberCount"`
			VideoCount      string `json:"videoCount"`
		} `json:"statistics"`
	} `json:"items"`
	Error *ytAPIError `json:"error,omitempty"`
}

type searchResp struct {
	Items []struct {
		ID struct {
			VideoID string `json:"videoId"`
		} `json:"id"`
	} `json:"items"`
	Error *ytAPIError `json:"error,omitempty"`
}

type videosResp struct {
	Items []struct {
		ID      string `json:"id"`
		Snippet struct {
			PublishedAt time.Time `json:"publishedAt"`
			Title       string    `json:"title"`
		} `json:"snippet"`
		Statistics struct {
			ViewCount    string `json:"viewCount"`
			LikeCount    string `json:"likeCount"`
			CommentCount string `json:"commentCount"`
		} `json:"statistics"`
	} `json:"items"`
	Error *ytAPIError `json:"error,omitempty"`
}

func (p *YouTubeParser) fetchChannelAPI(ctx context.Context, h string) (shared.ChannelSnapshot, error) {
	q := url.Values{}
	q.Set("part", "statistics,snippet")
	q.Set("forHandle", "@"+h)
	q.Set("key", p.cfg.APIKey)

	var resp channelsResp
	if err := p.getJSON(ctx, apiBase+"/channels?"+q.Encode(), &resp); err != nil {
		return shared.ChannelSnapshot{}, err
	}
	if resp.Error != nil {
		return shared.ChannelSnapshot{}, mapAPIError(resp.Error)
	}
	if len(resp.Items) == 0 {
		return shared.ChannelSnapshot{}, fmt.Errorf("youtube: %w", shared.ErrNotFound)
	}
	it := resp.Items[0]
	return shared.ChannelSnapshot{
		Platform:   shared.PlatformYouTube,
		Handle:     h,
		URL:        "https://www.youtube.com/@" + h,
		FetchedAt:  time.Now().UTC(),
		Followers:  mustAtoi(it.Statistics.SubscriberCount),
		PostsCount: mustAtoi(it.Statistics.VideoCount),
		TotalViews: mustAtoi(it.Statistics.ViewCount),
		Raw: map[string]interface{}{
			"channel_id": it.ID,
			"title":      it.Snippet.Title,
			"source":     "api",
		},
	}, nil
}

func (p *YouTubeParser) fetchRecentPostsAPI(ctx context.Context, h string, since time.Time) ([]shared.PostSnapshot, error) {
	channel, err := p.fetchChannelAPI(ctx, h)
	if err != nil {
		return nil, err
	}
	channelID, _ := channel.Raw["channel_id"].(string)
	if channelID == "" {
		return nil, fmt.Errorf("youtube: missing channel id")
	}

	q := url.Values{}
	q.Set("part", "id")
	q.Set("channelId", channelID)
	q.Set("type", "video")
	q.Set("order", "date")
	q.Set("maxResults", "50")
	if !since.IsZero() {
		q.Set("publishedAfter", since.UTC().Format(time.RFC3339))
	}
	q.Set("key", p.cfg.APIKey)

	var sr searchResp
	if err := p.getJSON(ctx, apiBase+"/search?"+q.Encode(), &sr); err != nil {
		return nil, err
	}
	if sr.Error != nil {
		return nil, mapAPIError(sr.Error)
	}
	if len(sr.Items) == 0 {
		return nil, nil
	}

	ids := make([]string, 0, len(sr.Items))
	for _, it := range sr.Items {
		if it.ID.VideoID != "" {
			ids = append(ids, it.ID.VideoID)
		}
	}

	vq := url.Values{}
	vq.Set("part", "statistics,snippet")
	vq.Set("id", strings.Join(ids, ","))
	vq.Set("key", p.cfg.APIKey)

	var vr videosResp
	if err := p.getJSON(ctx, apiBase+"/videos?"+vq.Encode(), &vr); err != nil {
		return nil, err
	}
	if vr.Error != nil {
		return nil, mapAPIError(vr.Error)
	}

	now := time.Now().UTC()
	out := make([]shared.PostSnapshot, 0, len(vr.Items))
	for _, v := range vr.Items {
		out = append(out, shared.PostSnapshot{
			Platform:      shared.PlatformYouTube,
			ChannelHandle: channel.Handle,
			PostID:        v.ID,
			URL:           "https://www.youtube.com/watch?v=" + v.ID,
			Kind:          shared.PostKindVideo,
			PublishedAt:   v.Snippet.PublishedAt,
			FetchedAt:     now,
			Likes:         mustAtoi(v.Statistics.LikeCount),
			Views:         mustAtoi(v.Statistics.ViewCount),
			Comments:      mustAtoi(v.Statistics.CommentCount),
			Raw:           map[string]interface{}{"title": v.Snippet.Title, "source": "api"},
		})
	}
	return out, nil
}

type ytAPIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

func mapAPIError(e *ytAPIError) error {
	switch e.Code {
	case 401, 403:
		if strings.Contains(strings.ToLower(e.Message), "quota") {
			return fmt.Errorf("youtube: %w: %s", shared.ErrRateLimited, e.Message)
		}
		return fmt.Errorf("youtube: %w: %s", shared.ErrAuth, e.Message)
	case 404:
		return fmt.Errorf("youtube: %w", shared.ErrNotFound)
	case 429:
		return fmt.Errorf("youtube: %w", shared.ErrRateLimited)
	case 500, 502, 503, 504:
		return fmt.Errorf("youtube: %w: %s", shared.ErrTransient, e.Message)
	default:
		return fmt.Errorf("youtube api %d: %s", e.Code, e.Message)
	}
}

func (p *YouTubeParser) getJSON(ctx context.Context, u string, dst interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("youtube: %w: %v", shared.ErrTransient, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("youtube: %w: http %d", shared.ErrTransient, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func mustAtoi(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// Compile-time interface check.
var _ shared.Parser = (*YouTubeParser)(nil)
