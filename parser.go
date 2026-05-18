// Package parser implements the w_popularity YouTube adapter using the
// YouTube Data API v3.
//
// Endpoints used (all GET, JSON):
//   - channels?forHandle=@h&part=statistics,snippet
//   - search?channelId=CID&type=video&order=date&publishedAfter=ISO
//   - videos?id=v1,v2,...&part=statistics,snippet
//
// Strategy:
//
//	primary:  YouTube Data API v3 (requires API key)
//	fallback: yt-dlp (not implemented yet)
package parser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	shared "github.com/suenot/w-popularity-shared"
)

const apiBase = "https://www.googleapis.com/youtube/v3"

// Config controls runtime behaviour.
type Config struct {
	APIKey      string
	HTTPClient  *http.Client
	HTTPTimeout time.Duration
}

func New(cfg Config) *YouTubeParser {
	if cfg.HTTPClient == nil {
		t := cfg.HTTPTimeout
		if t == 0 {
			t = 15 * time.Second
		}
		cfg.HTTPClient = &http.Client{Timeout: t}
	}
	return &YouTubeParser{cfg: cfg}
}

type YouTubeParser struct{ cfg Config }

func (p *YouTubeParser) Platform() shared.Platform { return shared.PlatformYouTube }

// --- Channel ---

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

func (p *YouTubeParser) FetchChannel(ctx context.Context, handle string) (shared.ChannelSnapshot, error) {
	if p.cfg.APIKey == "" {
		return shared.ChannelSnapshot{}, fmt.Errorf("youtube: %w", shared.ErrAuth)
	}

	h := strings.TrimPrefix(handle, "@")
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
	subs := mustAtoi(it.Statistics.SubscriberCount)
	views := mustAtoi(it.Statistics.ViewCount)
	videos := mustAtoi(it.Statistics.VideoCount)

	return shared.ChannelSnapshot{
		Platform:   shared.PlatformYouTube,
		Handle:     h,
		URL:        "https://www.youtube.com/@" + h,
		FetchedAt:  time.Now().UTC(),
		Followers:  subs,
		PostsCount: videos,
		TotalViews: views,
		Raw: map[string]interface{}{
			"channel_id": it.ID,
			"title":      it.Snippet.Title,
		},
	}, nil
}

// --- Posts (videos) ---

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

func (p *YouTubeParser) FetchRecentPosts(ctx context.Context, handle string, since time.Time) ([]shared.PostSnapshot, error) {
	if p.cfg.APIKey == "" {
		return nil, fmt.Errorf("youtube: %w", shared.ErrAuth)
	}

	channel, err := p.FetchChannel(ctx, handle)
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
			Raw:           map[string]interface{}{"title": v.Snippet.Title},
		})
	}
	return out, nil
}

// --- HTTP helpers ---

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

// guard against unused import warning if shared.Errors set shrinks.
var _ = errors.Is
