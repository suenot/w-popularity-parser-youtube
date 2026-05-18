package parser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	shared "github.com/suenot/w-popularity-shared"
)

func TestFetchChannel_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/channels") {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"items": []map[string]interface{}{{
				"id":      "UC123",
				"snippet": map[string]string{"title": "MarketMaker", "customUrl": "@mm"},
				"statistics": map[string]string{
					"viewCount":       "1000000",
					"subscriberCount": "50000",
					"videoCount":      "120",
				},
			}},
		})
	}))
	defer srv.Close()

	p := newWithBase(t, srv.URL)
	snap, err := p.FetchChannel(context.Background(), "@mm")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Followers != 50000 || snap.PostsCount != 120 || snap.TotalViews != 1000000 {
		t.Fatalf("unexpected stats: %+v", snap)
	}
	if snap.Platform != shared.PlatformYouTube {
		t.Fatalf("platform: %s", snap.Platform)
	}
}

func TestFetchChannel_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	p := newWithBase(t, srv.URL)
	_, err := p.FetchChannel(context.Background(), "@nope")
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestFetchChannel_NoKey(t *testing.T) {
	p := New(Config{}) // no APIKey
	_, err := p.FetchChannel(context.Background(), "@mm")
	if !errors.Is(err, shared.ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
}

func TestFetchChannel_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"quotaExceeded"}}`))
	}))
	defer srv.Close()

	p := newWithBase(t, srv.URL)
	_, err := p.FetchChannel(context.Background(), "@mm")
	if !errors.Is(err, shared.ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
}

// Helper: build a parser pointed at a test server by overriding the package
// constant via a tiny shim. We can't change apiBase from outside, so we use
// httptest.Server + URL replacement using a custom RoundTripper.
func newWithBase(t *testing.T, base string) *YouTubeParser {
	t.Helper()
	target := strings.TrimSuffix(base, "/")
	client := &http.Client{
		Transport: rewriteRT{target: target, next: http.DefaultTransport},
		Timeout:   3 * time.Second,
	}
	return New(Config{APIKey: "k", HTTPClient: client})
}

type rewriteRT struct {
	target string
	next   http.RoundTripper
}

func (r rewriteRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasPrefix(req.URL.String(), apiBase) {
		newURL := r.target + strings.TrimPrefix(req.URL.String(), apiBase)
		req2, err := http.NewRequestWithContext(req.Context(), req.Method, newURL, req.Body)
		if err != nil {
			return nil, err
		}
		req2.Header = req.Header
		return r.next.RoundTrip(req2)
	}
	return r.next.RoundTrip(req)
}
