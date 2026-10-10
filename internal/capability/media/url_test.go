package media

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"watchink/internal/app"
)

func TestNormalizeURL(t *testing.T) {
	const yt = "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	tests := []struct {
		in        string
		wantKey   string
		wantURL   string
		wantPID   string
		wantType  string
		wantCode  app.Code
		wantShort bool
	}{
		// YouTube forms.
		{in: "https://www.youtube.com/watch?v=dQw4w9WgXcQ", wantKey: "youtube:dQw4w9WgXcQ", wantURL: yt, wantPID: "dQw4w9WgXcQ", wantType: app.SourceYouTube},
		{in: "http://youtube.com/watch?v=dQw4w9WgXcQ&list=PL123&index=4&t=42s", wantKey: "youtube:dQw4w9WgXcQ", wantURL: yt},
		{in: "https://www.youtube.com/watch?list=PL123&v=dQw4w9WgXcQ", wantKey: "youtube:dQw4w9WgXcQ", wantURL: yt},
		{in: "https://m.youtube.com/watch?v=dQw4w9WgXcQ", wantKey: "youtube:dQw4w9WgXcQ", wantURL: yt},
		{in: "https://music.youtube.com/watch?v=dQw4w9WgXcQ&feature=share", wantKey: "youtube:dQw4w9WgXcQ", wantURL: yt},
		{in: "https://youtu.be/dQw4w9WgXcQ?si=abc&t=10", wantKey: "youtube:dQw4w9WgXcQ", wantURL: yt},
		{in: "youtu.be/dQw4w9WgXcQ", wantKey: "youtube:dQw4w9WgXcQ", wantURL: yt},
		{in: "https://www.youtube.com/shorts/dQw4w9WgXcQ", wantKey: "youtube:dQw4w9WgXcQ", wantURL: yt},
		{in: "https://www.youtube.com/live/dQw4w9WgXcQ?feature=share", wantKey: "youtube:dQw4w9WgXcQ", wantURL: yt},
		{in: "https://www.youtube.com/embed/dQw4w9WgXcQ?start=3", wantKey: "youtube:dQw4w9WgXcQ", wantURL: yt},
		{in: "https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ", wantKey: "youtube:dQw4w9WgXcQ", wantURL: yt},
		{in: "  HTTPS://WWW.YouTube.com/watch?v=a-b_c1D2e3F  ", wantKey: "youtube:a-b_c1D2e3F", wantURL: "https://www.youtube.com/watch?v=a-b_c1D2e3F"},
		{in: "https://www.youtube.com:443/watch?v=dQw4w9WgXcQ", wantKey: "youtube:dQw4w9WgXcQ", wantURL: yt},
		// YouTube links without a single video.
		{in: "https://www.youtube.com/playlist?list=PL123", wantCode: app.CodeUnsupportedSource},
		{in: "https://www.youtube.com/watch?list=PL123", wantCode: app.CodeUnsupportedSource},
		{in: "https://www.youtube.com/embed/videoseries?list=PL123", wantCode: app.CodeUnsupportedSource},
		{in: "https://www.youtube.com/@somechannel", wantCode: app.CodeUnsupportedSource},
		{in: "https://www.youtube.com/", wantCode: app.CodeUnsupportedSource},
		{in: "https://www.youtube.com/watch?v=tooshort", wantCode: app.CodeUnsupportedSource},
		{in: "https://www.youtube.com/watch?v=dQw4w9WgXcQ--extra", wantCode: app.CodeUnsupportedSource},
		{in: "https://youtu.be/", wantCode: app.CodeUnsupportedSource},

		// Bilibili.
		{in: "https://www.bilibili.com/video/BV17x411w7KC", wantKey: "bilibili:BV17x411w7KC:p1", wantURL: "https://www.bilibili.com/video/BV17x411w7KC?p=1", wantPID: "BV17x411w7KC:p1", wantType: app.SourceBilibili},
		{in: "https://www.bilibili.com/video/BV17x411w7KC/?spm_id_from=333.1007&vd_source=abc&p=3", wantKey: "bilibili:BV17x411w7KC:p3", wantURL: "https://www.bilibili.com/video/BV17x411w7KC?p=3", wantPID: "BV17x411w7KC:p3"},
		{in: "https://m.bilibili.com/video/BV17x411w7KC?p=1", wantKey: "bilibili:BV17x411w7KC:p1", wantURL: "https://www.bilibili.com/video/BV17x411w7KC?p=1"},
		{in: "bilibili.com/video/bv17x411w7KC", wantKey: "bilibili:BV17x411w7KC:p1", wantURL: "https://www.bilibili.com/video/BV17x411w7KC?p=1"},
		{in: "https://www.bilibili.com/video/av170001", wantKey: "bilibili:BV17x411w7KC:p1", wantURL: "https://www.bilibili.com/video/BV17x411w7KC?p=1"},
		{in: "https://www.bilibili.com/video/av170001/?p=2", wantKey: "bilibili:BV17x411w7KC:p2", wantURL: "https://www.bilibili.com/video/BV17x411w7KC?p=2"},
		{in: "https://b23.tv/BV17x411w7KC?p=2", wantKey: "bilibili:BV17x411w7KC:p2", wantURL: "https://www.bilibili.com/video/BV17x411w7KC?p=2"},
		{in: "https://b23.tv/av170001", wantKey: "bilibili:BV17x411w7KC:p1", wantURL: "https://www.bilibili.com/video/BV17x411w7KC?p=1"},
		{in: "https://www.bilibili.com/video/BV17x411w7KC?p=0", wantCode: app.CodeInvalidArgument},
		{in: "https://www.bilibili.com/video/BV17x411w7KC?p=abc", wantCode: app.CodeInvalidArgument},
		{in: "https://www.bilibili.com/video/BV17x411w7K", wantCode: app.CodeUnsupportedSource},
		{in: "https://www.bilibili.com/video/BV17x411w7KO", wantCode: app.CodeUnsupportedSource}, // O is not base58
		{in: "https://www.bilibili.com/bangumi/play/ep12345", wantCode: app.CodeUnsupportedSource},
		{in: "https://live.bilibili.com/12345", wantCode: app.CodeUnsupportedSource},
		{in: "https://space.bilibili.com/1/channel/collectiondetail?sid=2", wantCode: app.CodeUnsupportedSource},
		{in: "https://www.bilibili.com/", wantCode: app.CodeUnsupportedSource},

		// b23.tv short links need the network.
		{in: "https://b23.tv/AbCd123", wantURL: "https://b23.tv/AbCd123", wantType: app.SourceBilibili, wantShort: true},
		{in: "http://b23.tv/AbCd123?share_source=copy", wantURL: "https://b23.tv/AbCd123", wantShort: true},
		{in: "https://b23.tv/", wantCode: app.CodeUnsupportedSource},
		{in: "https://b23.tv/a/b", wantCode: app.CodeUnsupportedSource},

		// Everything else.
		{in: "https://vimeo.com/12345", wantCode: app.CodeUnsupportedSource},
		{in: "https://youtube.com.evil.example/watch?v=dQw4w9WgXcQ", wantCode: app.CodeUnsupportedSource},
		{in: "https://evil.example/?u=https://www.youtube.com/watch?v=dQw4w9WgXcQ", wantCode: app.CodeUnsupportedSource},
		{in: "https://user:pw@www.youtube.com/watch?v=dQw4w9WgXcQ", wantCode: app.CodeUnsupportedSource},
		{in: "ftp://www.youtube.com/watch?v=dQw4w9WgXcQ", wantCode: app.CodeUnsupportedSource},
		{in: "file:///etc/passwd", wantCode: app.CodeInvalidArgument},
		{in: "", wantCode: app.CodeInvalidArgument},
		{in: "https://www.youtube.com/watch?v=dQw4w9WgXcQ --exec evil", wantCode: app.CodeInvalidArgument},
		{in: "--exec", wantCode: app.CodeUnsupportedSource},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			ref, err := NormalizeURL(tt.in)
			if tt.wantCode != "" {
				if !app.IsCode(err, tt.wantCode) {
					t.Fatalf("err = %v (ref %+v), want %s", err, ref, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ref.Key != tt.wantKey || ref.CanonicalURL != tt.wantURL || ref.NeedsResolve != tt.wantShort {
				t.Errorf("got %+v, want key %q url %q short %v", ref, tt.wantKey, tt.wantURL, tt.wantShort)
			}
			if tt.wantPID != "" && ref.PlatformID != tt.wantPID {
				t.Errorf("PlatformID = %q, want %q", ref.PlatformID, tt.wantPID)
			}
			if tt.wantType != "" && ref.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", ref.Type, tt.wantType)
			}
			// The canonical URL must normalize to itself.
			again, err := NormalizeURL(ref.CanonicalURL)
			if err != nil || again.Key != ref.Key || again.CanonicalURL != ref.CanonicalURL {
				t.Errorf("canonical URL is not a fixed point: %+v, %v", again, err)
			}
		})
	}
}

func TestAVBVConversion(t *testing.T) {
	pairs := []struct {
		av uint64
		bv string
	}{
		{170001, "BV17x411w7KC"},
		{2, "BV1xx411c7mD"},
		{455017605, "BV1Q541167Qg"},
		{882584971, "BV1mK4y1C7Bz"},
	}
	for _, p := range pairs {
		if got, err := AVToBV(p.av); err != nil || got != p.bv {
			t.Errorf("AVToBV(%d) = %q, %v; want %q", p.av, got, err, p.bv)
		}
	}
	// A spread of values, including the post-2^32 range, yields well-formed ids.
	for _, av := range []uint64{1, 99, 1 << 20, 1<<32 + 7, 1<<40 + 12345, 1<<51 - 1} {
		bv, err := AVToBV(av)
		if err != nil || !reBVID.MatchString(bv) {
			t.Fatalf("AVToBV(%d) = %q, %v", av, bv, err)
		}
	}
	for _, av := range []uint64{0, 1 << 51, 1 << 60} {
		if _, err := AVToBV(av); err == nil {
			t.Errorf("AVToBV(%d) accepted an out-of-range value", av)
		}
	}
}

// roundTripFunc fakes b23.tv without any network access.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func redirectTo(loc string) *http.Response {
	return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {loc}}, Body: io.NopCloser(strings.NewReader(""))}
}

func status(code int) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}
}

func TestResolveShortLink(t *testing.T) {
	tests := []struct {
		name     string
		routes   map[string]*http.Response
		fail     error
		in       string
		wantKey  string
		wantCode app.Code
	}{
		{
			name:    "direct redirect to video",
			routes:  map[string]*http.Response{"https://b23.tv/AbCd123": redirectTo("https://www.bilibili.com/video/BV17x411w7KC?p=2&share_source=copy_web")},
			in:      "https://b23.tv/AbCd123",
			wantKey: "bilibili:BV17x411w7KC:p2",
		},
		{
			name: "two hops and av number",
			routes: map[string]*http.Response{
				"https://b23.tv/first":  redirectTo("https://b23.tv/second"),
				"https://b23.tv/second": redirectTo("https://m.bilibili.com/video/av170001"),
			},
			in:      "b23.tv/first",
			wantKey: "bilibili:BV17x411w7KC:p1",
		},
		{
			name:    "no request needed for a BV path",
			in:      "https://b23.tv/BV17x411w7KC",
			wantKey: "bilibili:BV17x411w7KC:p1",
		},
		{
			name:     "redirect to a non-video page",
			routes:   map[string]*http.Response{"https://b23.tv/AbCd123": redirectTo("https://www.bilibili.com/bangumi/play/ep1")},
			in:       "https://b23.tv/AbCd123",
			wantCode: app.CodeUnsupportedSource,
		},
		{
			name:     "redirect to a foreign host is not followed",
			routes:   map[string]*http.Response{"https://b23.tv/AbCd123": redirectTo("https://evil.example/video/BV17x411w7KC")},
			in:       "https://b23.tv/AbCd123",
			wantCode: app.CodeUnsupportedSource,
		},
		{
			name:     "redirect to youtube is rejected",
			routes:   map[string]*http.Response{"https://b23.tv/AbCd123": redirectTo("https://www.youtube.com/watch?v=dQw4w9WgXcQ")},
			in:       "https://b23.tv/AbCd123",
			wantCode: app.CodeUnsupportedSource,
		},
		{
			name:     "missing short link",
			routes:   map[string]*http.Response{"https://b23.tv/AbCd123": status(http.StatusNotFound)},
			in:       "https://b23.tv/AbCd123",
			wantCode: app.CodeSourceUnavailable,
		},
		{
			name:     "page without redirect",
			routes:   map[string]*http.Response{"https://b23.tv/AbCd123": status(http.StatusOK)},
			in:       "https://b23.tv/AbCd123",
			wantCode: app.CodeUnsupportedSource,
		},
		{
			name:     "redirect loop",
			routes:   map[string]*http.Response{"https://b23.tv/loop": redirectTo("https://b23.tv/loop")},
			in:       "https://b23.tv/loop",
			wantCode: app.CodeUnsupportedSource,
		},
		{
			name:     "network failure",
			fail:     errors.New("dial tcp: connection refused"),
			in:       "https://b23.tv/AbCd123",
			wantCode: app.CodeNetworkError,
		},
		{
			name:     "not a short link host",
			in:       "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
			wantCode: app.CodeUnsupportedSource,
		},
		{
			name:     "unsupported host",
			in:       "https://example.com/x",
			wantCode: app.CodeUnsupportedSource,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() != "b23.tv" {
					t.Errorf("request left b23.tv: %s", r.URL)
				}
				if len(r.Cookies()) > 0 {
					t.Errorf("cookies sent: %v", r.Cookies())
				}
				if tt.fail != nil {
					return nil, tt.fail
				}
				resp, ok := tt.routes[r.URL.String()]
				if !ok {
					t.Errorf("unexpected request %s", r.URL)
					return status(http.StatusNotFound), nil
				}
				return resp, nil
			})}
			ref, err := ResolveShortLink(context.Background(), client, tt.in)
			if tt.wantCode != "" {
				if !app.IsCode(err, tt.wantCode) {
					t.Fatalf("err = %v (ref %+v), want %s", err, ref, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ref.Key != tt.wantKey || ref.NeedsResolve || ref.Type != app.SourceBilibili {
				t.Errorf("ref = %+v, want key %q", ref, tt.wantKey)
			}
		})
	}
}

func TestResolveShortLinkCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, r.Context().Err()
	})}
	if _, err := ResolveShortLink(ctx, client, "https://b23.tv/AbCd123"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
