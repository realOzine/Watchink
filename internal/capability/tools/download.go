package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"watchink/internal/app"
)

const userAgent = "Watchink-tools/0"

// SetNetwork replaces the mirror table and proxy. It may be called at
// any time; downloads started afterwards use the new values. An empty proxy
// means "follow HTTPS_PROXY and friends".
func (m *Manager) SetNetwork(mirrors map[string]string, proxy string) {
	cp := make(map[string]string, len(mirrors))
	for host, base := range mirrors {
		cp[strings.ToLower(strings.TrimSpace(host))] = strings.TrimSpace(base)
	}
	m.netMu.Lock()
	m.mirrors, m.proxy = cp, strings.TrimSpace(proxy)
	m.netMu.Unlock()
}

func (m *Manager) proxyFor(req *http.Request) (*url.URL, error) {
	m.netMu.RLock()
	p := m.proxy
	m.netMu.RUnlock()
	if p == "" {
		return http.ProxyFromEnvironment(req)
	}
	return url.Parse(p)
}

// mirrorURL rewrites raw when its host has a mirror: the mirror base replaces
// scheme and host, path and query are kept. The sha256 check is unchanged, so
// a mirror cannot alter what gets installed.
func (m *Manager) mirrorURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	m.netMu.RLock()
	base, ok := m.mirrors[strings.ToLower(u.Host)]
	if !ok {
		base, ok = m.mirrors[strings.ToLower(u.Hostname())]
	}
	m.netMu.RUnlock()
	if !ok || base == "" {
		return raw
	}
	out := strings.TrimRight(base, "/") + u.EscapedPath()
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

func urlPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Path
}

func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

func networkErr(rawURL string, err error, format string, args ...any) *app.Error {
	e := app.E(app.CodeNetworkError, format, args...).
		WithHint("请检查网络连接；可在设置中配置代理（network.proxy）或下载镜像（download.mirrors），也可以手动下载后用 watchink tools import 导入。").
		WithDetail("host", urlHost(rawURL))
	if err != nil {
		e = e.WithCause(err)
	}
	return e
}

// download fetches rawURL (after mirror rewriting) into dest and verifies it.
// dest is a ".part" file that survives failures so a later call resumes with
// a Range request; it is deleted when the content turns out to be wrong. On
// success dest holds exactly the bytes whose sha256 is wantSHA.
func (m *Manager) download(ctx context.Context, rawURL, wantSHA string, wantSize int64, dest string, report func(done, total int64)) error {
	target := m.mirrorURL(rawURL)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return ioErr(err, "创建下载目录")
	}
	var last error
	for attempt := 0; attempt < m.attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(m.retryDelay << (attempt - 1)):
			}
		}
		retry, err := m.downloadOnce(ctx, target, wantSize, dest, report)
		if err == nil {
			got, size, herr := hashFile(dest)
			if herr != nil {
				return ioErr(herr, "校验下载文件")
			}
			if got != wantSHA {
				os.Remove(dest)
				return app.E(app.CodeChecksumMismatch, "下载的文件校验失败：%s", path.Base(urlPath(rawURL))).
					WithHint("文件可能损坏或被篡改，已删除。请重试；如果使用了镜像，请检查镜像是否可信。").
					WithDetail("host", urlHost(target)).WithDetail("want_sha256", wantSHA).
					WithDetail("got_sha256", got).WithDetail("size", size)
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !retry {
			return err
		}
		last = err
		m.log.Warn("tools: download attempt failed", "host", urlHost(target), "attempt", attempt+1, "err", err)
	}
	return last
}

// downloadOnce performs one HTTP request, appending to dest when the server
// honours the Range header. retry reports whether another attempt may help.
func (m *Manager) downloadOnce(ctx context.Context, target string, wantSize int64, dest string, report func(done, total int64)) (retry bool, err error) {
	var offset int64
	if st, statErr := os.Stat(dest); statErr == nil {
		offset = st.Size()
	}
	if wantSize > 0 && offset > wantSize {
		os.Remove(dest)
		offset = 0
	}
	if wantSize > 0 && offset == wantSize {
		report(offset, wantSize)
		return false, nil // complete from an earlier run; the caller verifies
	}

	// A transfer that stops delivering bytes is aborted so the retry loop can
	// resume it, instead of hanging until the peer gives up.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := time.AfterFunc(m.idleTimeout, cancel)
	defer idle.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, networkErr(target, err, "下载地址无效")
	}
	req.Header.Set("User-Agent", userAgent)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return true, networkErr(target, err, "下载失败：%v", sanitizeNetErr(err))
	}
	defer resp.Body.Close()

	flags := os.O_WRONLY | os.O_CREATE
	total := wantSize
	switch {
	case resp.StatusCode == http.StatusOK:
		offset = 0
		flags |= os.O_TRUNC
		if resp.ContentLength > 0 {
			total = resp.ContentLength
		}
	case resp.StatusCode == http.StatusPartialContent:
		if !strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", offset)) {
			os.Remove(dest)
			return true, networkErr(target, nil, "服务器返回了错误的续传范围")
		}
		flags |= os.O_APPEND
		if resp.ContentLength > 0 {
			total = offset + resp.ContentLength
		}
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && offset > 0:
		if wantSize == 0 {
			return false, nil // nothing left to fetch; the caller verifies
		}
		os.Remove(dest)
		return true, networkErr(target, nil, "续传失败，重新下载")
	default:
		// 5xx, 408 and 429 are worth another try; other 4xx are not.
		retry := resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests
		return retry, networkErr(target, nil, "下载失败：服务器返回 HTTP %d", resp.StatusCode).WithDetail("http_status", resp.StatusCode)
	}

	f, err := os.OpenFile(dest, flags, 0o644)
	if err != nil {
		return false, ioErr(err, "写入下载文件")
	}
	done := offset
	report(done, total)
	buf := make([]byte, 256<<10)
	lastReport := time.Now()
	var copyErr error
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			idle.Reset(m.idleTimeout)
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return false, ioErr(werr, "写入下载文件")
			}
			done += int64(n)
			if wantSize > 0 && done > wantSize {
				// More data than the manifest allows can never verify; stop
				// before a bad server fills the disk.
				f.Close()
				os.Remove(dest)
				return false, app.E(app.CodeChecksumMismatch, "下载的文件比清单记录的更大，已中止").
					WithDetail("host", urlHost(target))
			}
			if time.Since(lastReport) >= 150*time.Millisecond {
				lastReport = time.Now()
				report(done, total)
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				copyErr = rerr
			}
			break
		}
	}
	if err := f.Close(); err != nil {
		return false, ioErr(err, "写入下载文件")
	}
	if copyErr != nil {
		return true, networkErr(target, copyErr, "下载中断：%v", sanitizeNetErr(copyErr))
	}
	if total > 0 && done < total {
		return true, networkErr(target, nil, "下载不完整（%d / %d 字节）", done, total)
	}
	report(done, done)
	return false, nil
}

// sanitizeNetErr drops the URL from *url.Error: a proxy or mirror URL may
// carry credentials, and the host is already in the error details.
func sanitizeNetErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// getSmall fetches a small text resource (a checksum list) into memory.
func (m *Manager) getSmall(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	target := m.mirrorURL(rawURL)
	ctx, cancel := context.WithTimeout(ctx, 2*m.idleTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, networkErr(target, err, "下载地址无效")
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, networkErr(target, err, "请求失败：%v", sanitizeNetErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, networkErr(target, nil, "请求失败：服务器返回 HTTP %d", resp.StatusCode).WithDetail("http_status", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, networkErr(target, err, "请求中断：%v", sanitizeNetErr(err))
	}
	if int64(len(data)) > limit {
		return nil, networkErr(target, nil, "响应过大")
	}
	return data, nil
}

// latestTag resolves a ".../releases/latest" URL to the tag it redirects to,
// without using the rate-limited GitHub API.
func (m *Manager) latestTag(ctx context.Context, rawURL string) (string, error) {
	target := m.mirrorURL(rawURL)
	client := *m.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx, cancel := context.WithTimeout(ctx, 2*m.idleTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", networkErr(target, err, "更新地址无效")
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", networkErr(target, err, "查询最新版本失败：%v", sanitizeNetErr(err))
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode < 300 || resp.StatusCode > 399 || loc == "" {
		return "", networkErr(target, nil, "查询最新版本失败：服务器返回 HTTP %d", resp.StatusCode).WithDetail("http_status", resp.StatusCode)
	}
	tag := path.Base(urlPath(loc))
	if !versionRE.MatchString(tag) {
		return "", networkErr(target, nil, "查询最新版本失败：无法识别的版本号")
	}
	return tag, nil
}
