package parser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	shared "github.com/suenot/w-popularity-shared"
)

// writeFakeYTDLP writes an executable shell script to a temp dir that emits
// `payload` to stdout and returns `exitCode`. `stderr` is written verbatim to
// the script's stderr. It returns the absolute path of the script.
func writeFakeYTDLP(t *testing.T, payload, stderrMsg string, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake yt-dlp shell script only runs on unix")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "yt-dlp")

	// Write payload and stderr to sibling files; the script cats them.
	stdoutFile := filepath.Join(dir, "stdout.json")
	stderrFile := filepath.Join(dir, "stderr.txt")
	if err := os.WriteFile(stdoutFile, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderrFile, []byte(stderrMsg), 0o644); err != nil {
		t.Fatal(err)
	}

	script := "#!/bin/sh\n" +
		"cat " + stdoutFile + "\n" +
		"cat " + stderrFile + " 1>&2\n" +
		"exit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func itoa(n int) string {
	// avoid pulling in strconv just for testing.
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	buf := make([]byte, 0, 4)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		return "-" + string(buf)
	}
	return string(buf)
}

// --- FetchChannel via yt-dlp -------------------------------------------------

func TestFetchChannel_YTDLP_Happy(t *testing.T) {
	payload := `{
		"id": "@mm",
		"channel_id": "UC123",
		"title": "MarketMaker",
		"channel_follower_count": 50000,
		"view_count": 1000000,
		"playlist_count": 120
	}`
	bin := writeFakeYTDLP(t, payload, "", 0)

	p := New(Config{YTDLPPath: bin})
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
	if snap.Followers != 50000 || snap.PostsCount != 120 || snap.TotalViews != 1000000 {
		t.Fatalf("counts: %+v", snap)
	}
	if cid, _ := snap.Raw["channel_id"].(string); cid != "UC123" {
		t.Fatalf("missing channel_id: %+v", snap.Raw)
	}
	if src, _ := snap.Raw["source"].(string); src != "yt-dlp" {
		t.Fatalf("source: %v", snap.Raw["source"])
	}
}

func TestFetchChannel_YTDLP_PrivateNotFound(t *testing.T) {
	bin := writeFakeYTDLP(t, "", "ERROR: This channel does not exist.\n", 1)
	p := New(Config{YTDLPPath: bin})
	_, err := p.FetchChannel(context.Background(), "@nope")
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestFetchChannel_YTDLP_RateLimited(t *testing.T) {
	bin := writeFakeYTDLP(t, "", "ERROR: HTTP Error 429: Too Many Requests\n", 1)
	p := New(Config{YTDLPPath: bin})
	_, err := p.FetchChannel(context.Background(), "@mm")
	if !errors.Is(err, shared.ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
}

func TestFetchChannel_YTDLP_TransientFailure(t *testing.T) {
	bin := writeFakeYTDLP(t, "", "ERROR: Something exploded internally\n", 1)
	p := New(Config{YTDLPPath: bin})
	_, err := p.FetchChannel(context.Background(), "@mm")
	if !errors.Is(err, shared.ErrTransient) {
		t.Fatalf("want ErrTransient, got %v", err)
	}
}

// --- FetchRecentPosts via yt-dlp --------------------------------------------

func TestFetchRecentPosts_YTDLP_Happy(t *testing.T) {
	// Two videos: one fresh, one older than `since`.
	payload := `{
		"id": "@mm",
		"entries": [
			{
				"id": "vid1",
				"title": "fresh",
				"webpage_url": "https://www.youtube.com/watch?v=vid1",
				"upload_date": "20260510",
				"view_count": 1000,
				"like_count": 50,
				"comment_count": 7
			},
			{
				"id": "vid2",
				"title": "older",
				"upload_date": "20240101",
				"view_count": 999,
				"like_count": 9,
				"comment_count": 1
			}
		]
	}`
	bin := writeFakeYTDLP(t, payload, "", 0)

	p := New(Config{YTDLPPath: bin})
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	posts, err := p.FetchRecentPosts(context.Background(), "@mm", since)
	if err != nil {
		t.Fatalf("FetchRecentPosts: %v", err)
	}
	if len(posts) != 1 {
		t.Fatalf("want 1 post after since filter, got %d", len(posts))
	}
	got := posts[0]
	if got.PostID != "vid1" || got.Likes != 50 || got.Views != 1000 || got.Comments != 7 {
		t.Fatalf("post mismatch: %+v", got)
	}
	if got.Kind != shared.PostKindVideo {
		t.Fatalf("kind: %s", got.Kind)
	}
	if got.URL != "https://www.youtube.com/watch?v=vid1" {
		t.Fatalf("url: %s", got.URL)
	}
	if got.PublishedAt.IsZero() {
		t.Fatalf("published_at zero")
	}
	if got.ChannelHandle != "mm" {
		t.Fatalf("channel_handle: %s", got.ChannelHandle)
	}
}

func TestFetchRecentPosts_YTDLP_EmptyEntries(t *testing.T) {
	payload := `{"id":"@mm","entries":[]}`
	bin := writeFakeYTDLP(t, payload, "", 0)

	p := New(Config{YTDLPPath: bin})
	posts, err := p.FetchRecentPosts(context.Background(), "@mm", time.Time{})
	if err != nil {
		t.Fatalf("FetchRecentPosts: %v", err)
	}
	if len(posts) != 0 {
		t.Fatalf("want 0 posts, got %d", len(posts))
	}
}

// --- yt-dlp missing: ErrAuth when no API key --------------------------------

func TestFetchChannel_NoYTDLPAndNoAPIKey(t *testing.T) {
	// Force resolveYTDLP() to fail by isolating PATH and pointing YTDLPPath nowhere.
	t.Setenv("PATH", "/var/empty-doesnt-exist")
	p := New(Config{}) // no YTDLPPath, no APIKey
	_, err := p.FetchChannel(context.Background(), "@mm")
	if !errors.Is(err, shared.ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}

	_, err = p.FetchRecentPosts(context.Background(), "@mm", time.Time{})
	if !errors.Is(err, shared.ErrAuth) {
		t.Fatalf("want ErrAuth for posts, got %v", err)
	}
}

// --- Platform sanity check --------------------------------------------------

func TestPlatform(t *testing.T) {
	p := New(Config{})
	if p.Platform() != shared.PlatformYouTube {
		t.Fatalf("platform: %s", p.Platform())
	}
}
