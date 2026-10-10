package media

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"watchink/internal/app"
)

// SourceRef is a normalized link source: the dedupe key of §6.6 plus the
// canonical URL that is safe to hand to yt-dlp (§15).
type SourceRef struct {
	// Type is app.SourceYouTube or app.SourceBilibili.
	Type string `json:"type"`
	// Key is the source_key: "youtube:<id>" or "bilibili:<BV>:p<n>".
	Key string `json:"key"`
	// CanonicalURL is the normalized watch URL. For a short link that still
	// needs resolving it is the cleaned https://b23.tv/... URL.
	CanonicalURL string `json:"canonical_url"`
	// PlatformID is the 11-char id (YouTube) or "<BV>:p<n>" (Bilibili).
	PlatformID string `json:"platform_id"`
	// NeedsResolve marks a b23.tv short link: Key and PlatformID are empty
	// and the caller must call ResolveShortLink.
	NeedsResolve bool `json:"needs_resolve,omitempty"`
}

// shortLinkTimeout is the §6.6 limit for resolving a b23.tv link.
const shortLinkTimeout = 5 * time.Second

var (
	reYouTubeID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	// BV ids are "BV1" + 9 base58 characters.
	reBVID = regexp.MustCompile(`^[Bb][Vv]1[1-9A-HJ-NP-Za-km-z]{9}$`)
	reAVID = regexp.MustCompile(`^[Aa][Vv]([0-9]{1,16})$`)
	// b23.tv share codes; anything else on that host is not a video link.
	reShortCode = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)
)

func unsupported(format string, args ...any) *app.Error {
	return app.E(app.CodeUnsupportedSource, format, args...).
		WithHint("v0 只支持 YouTube 与 Bilibili 的单个视频链接。")
}

// NormalizeURL turns a user-supplied link into a SourceRef without touching
// the network (§6.6, D-13). Hosts other than YouTube / Bilibili, and links
// that do not identify a single video (playlists, channels, live rooms…),
// return UNSUPPORTED_SOURCE. A b23.tv short link returns NeedsResolve.
func NormalizeURL(raw string) (*SourceRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, app.E(app.CodeInvalidArgument, "链接不能为空")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return nil, app.E(app.CodeInvalidArgument, "链接中不能包含空白字符")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil, app.E(app.CodeInvalidArgument, "无法解析链接")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, unsupported("不支持的链接协议：%s", u.Scheme)
	}
	if u.User != nil {
		return nil, unsupported("不支持带用户信息的链接")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	segs := pathSegments(u.Path)

	switch host {
	case "youtube.com", "www.youtube.com", "m.youtube.com", "music.youtube.com",
		"youtube-nocookie.com", "www.youtube-nocookie.com":
		return normalizeYouTube(u, segs)
	case "youtu.be", "www.youtu.be":
		if len(segs) == 0 {
			return nil, unsupported("链接中没有视频 ID")
		}
		return youtubeRef(segs[0])
	case "bilibili.com", "www.bilibili.com", "m.bilibili.com":
		return normalizeBilibili(u, segs)
	case "b23.tv", "www.b23.tv":
		return normalizeShortLink(u, segs)
	}
	return nil, unsupported("不支持的视频来源：%s", host)
}

func pathSegments(p string) []string {
	var segs []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	return segs
}

func normalizeYouTube(u *url.URL, segs []string) (*SourceRef, error) {
	if len(segs) == 0 {
		return nil, unsupported("链接中没有视频 ID")
	}
	switch segs[0] {
	case "watch":
		// list= / index= / t= and friends are ignored on purpose (§6.6).
		v := u.Query().Get("v")
		if v == "" {
			return nil, unsupported("链接中没有视频 ID")
		}
		return youtubeRef(v)
	case "shorts", "live", "embed", "v":
		if len(segs) < 2 {
			return nil, unsupported("链接中没有视频 ID")
		}
		if segs[0] == "embed" && segs[1] == "videoseries" {
			// The playlist-only embed; "videoseries" happens to be 11 chars.
			return nil, unsupported("不支持播放列表链接，请提供单个视频的链接")
		}
		return youtubeRef(segs[1])
	case "playlist":
		return nil, unsupported("不支持播放列表链接，请提供单个视频的链接")
	}
	return nil, unsupported("不是 YouTube 视频链接")
}

func youtubeRef(id string) (*SourceRef, error) {
	if !reYouTubeID.MatchString(id) {
		// e.g. /embed/videoseries?list=... (a playlist-only embed).
		return nil, unsupported("链接中没有有效的 YouTube 视频 ID")
	}
	return &SourceRef{
		Type:         app.SourceYouTube,
		Key:          "youtube:" + id,
		CanonicalURL: "https://www.youtube.com/watch?v=" + id,
		PlatformID:   id,
	}, nil
}

func normalizeBilibili(u *url.URL, segs []string) (*SourceRef, error) {
	if len(segs) < 2 || segs[0] != "video" {
		return nil, unsupported("不是 Bilibili 视频链接（只支持 /video/BV… 或 /video/av…）")
	}
	return bilibiliRef(segs[1], u.Query().Get("p"))
}

func bilibiliRef(id, page string) (*SourceRef, error) {
	var bv string
	switch {
	case reBVID.MatchString(id):
		bv = "BV" + id[2:]
	case reAVID.MatchString(id):
		aid, err := strconv.ParseUint(id[2:], 10, 64)
		if err != nil {
			return nil, unsupported("无效的 av 号")
		}
		bv, err = AVToBV(aid)
		if err != nil {
			return nil, unsupported("无效的 av 号")
		}
	default:
		return nil, unsupported("链接中没有有效的 BV 号或 av 号")
	}
	p := 1
	if page != "" {
		n, err := strconv.Atoi(page)
		if err != nil || n < 1 || n > 100000 {
			return nil, app.E(app.CodeInvalidArgument, "无效的分 P 序号：%s", page)
		}
		p = n
	}
	pid := fmt.Sprintf("%s:p%d", bv, p)
	return &SourceRef{
		Type:         app.SourceBilibili,
		Key:          "bilibili:" + pid,
		CanonicalURL: fmt.Sprintf("https://www.bilibili.com/video/%s?p=%d", bv, p),
		PlatformID:   pid,
	}, nil
}

func normalizeShortLink(u *url.URL, segs []string) (*SourceRef, error) {
	if len(segs) != 1 {
		return nil, unsupported("不是有效的 b23.tv 短链")
	}
	// b23.tv/BV… and b23.tv/av… carry the id themselves: no network needed.
	if reBVID.MatchString(segs[0]) || reAVID.MatchString(segs[0]) {
		return bilibiliRef(segs[0], u.Query().Get("p"))
	}
	if !reShortCode.MatchString(segs[0]) {
		return nil, unsupported("不是有效的 b23.tv 短链")
	}
	return &SourceRef{
		Type:         app.SourceBilibili,
		CanonicalURL: "https://b23.tv/" + segs[0],
		NeedsResolve: true,
	}, nil
}

// av <-> BV conversion (the algorithm Bilibili has used since 2024, which
// covers av numbers up to 2^51).
const (
	bvXOR   uint64 = 23442827791579
	bvMask  uint64 = 2251799813685247 // 2^51 - 1
	bvMaxAV uint64 = 1 << 51
	bvTable        = "FcwAPNKTMug3GV5Lj7EJnHpWsx4tb8haYeviqBz6rkCy12mUSDQX9RdoZf"
)

// AVToBV converts an av number to its BV id.
func AVToBV(aid uint64) (string, error) {
	if aid == 0 || aid >= bvMaxAV {
		return "", fmt.Errorf("av number %d out of range", aid)
	}
	b := []byte("BV1000000000")
	tmp := (bvMaxAV | aid) ^ bvXOR
	for i := len(b) - 1; tmp > 0 && i >= 3; i-- {
		b[i] = bvTable[tmp%58]
		tmp /= 58
	}
	b[3], b[9] = b[9], b[3]
	b[4], b[7] = b[7], b[4]
	return string(b), nil
}

// ResolveShortLink resolves a b23.tv short link by following its redirects
// (at most 5 hops, 5 s in total — §6.6) and normalizes the target, which must
// be a Bilibili video. Only b23.tv itself is ever contacted: the walk stops
// at the first redirect that leaves that host. client may be nil.
func ResolveShortLink(ctx context.Context, client *http.Client, rawURL string) (*SourceRef, error) {
	ref, err := NormalizeURL(rawURL)
	if err != nil {
		return nil, err
	}
	if !ref.NeedsResolve {
		if ref.Type != app.SourceBilibili {
			return nil, unsupported("不是 b23.tv 短链")
		}
		return ref, nil
	}

	ctx, cancel := context.WithTimeout(ctx, shortLinkTimeout)
	defer cancel()
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if client != nil {
		hc.Transport, hc.Jar = client.Transport, nil // never send or store cookies
	}

	current := ref.CanonicalURL
	for hop := 0; hop < 5; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return nil, app.E(app.CodeInvalidArgument, "无法解析短链")
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15")
		resp, err := hc.Do(req)
		if err != nil {
			if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ctx.Err()
			}
			e := app.E(app.CodeNetworkError, "解析 b23.tv 短链失败").WithCause(err).
				WithHint("请检查网络或代理设置，或直接粘贴 bilibili.com 的完整视频链接。")
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				e.Message = "解析 b23.tv 短链超时"
			}
			return nil, e
		}
		loc := resp.Header.Get("Location")
		status := resp.StatusCode
		resp.Body.Close()

		if status < 300 || status > 399 || loc == "" {
			if status == http.StatusNotFound || status == http.StatusGone {
				return nil, app.E(app.CodeSourceUnavailable, "b23.tv 短链不存在或已失效")
			}
			return nil, unsupported("b23.tv 短链没有指向 Bilibili 视频")
		}
		next, err := req.URL.Parse(loc)
		if err != nil {
			return nil, unsupported("b23.tv 短链没有指向 Bilibili 视频")
		}
		target, err := NormalizeURL(next.String())
		if err != nil {
			// Bangumi, live rooms, articles … or a foreign host.
			return nil, unsupported("b23.tv 短链没有指向 Bilibili 视频")
		}
		if !target.NeedsResolve {
			if target.Type != app.SourceBilibili {
				return nil, unsupported("b23.tv 短链没有指向 Bilibili 视频")
			}
			return target, nil
		}
		current = target.CanonicalURL // another b23.tv hop
	}
	return nil, unsupported("b23.tv 短链重定向次数过多")
}
