package tools

import (
	"context"
	"path"
	"strings"

	"watchink/internal/app"
)

// UpdateYtDlp installs the latest official yt-dlp release and makes
// it the active version; the previous one stays installed for Rollback.
func (m *Manager) UpdateYtDlp(ctx context.Context) (version string, err error) {
	return m.UpdateTool(ctx, ToolYtDlp)
}

// UpdateTool installs the latest upstream release of a tool whose manifest
// entry has an update source. The release's own checksum list supplies the
// sha256, and the usual flow (verify, smoke test, atomic install) applies.
func (m *Manager) UpdateTool(ctx context.Context, name string) (version string, err error) {
	t, ok := m.manifest.Tools[name]
	if !ok {
		return "", errUnknownItem(name)
	}
	if t.Update == nil {
		return "", app.E(app.CodeInvalidArgument, "%s 不支持在线更新", name)
	}
	plat, ok := t.Platforms[m.platform]
	if !ok {
		return "", m.errNoPlatform(name)
	}
	err = m.exclusive(ctx, name, false, nil, func(report func(done, total int64)) error {
		tag, err := m.latestTag(ctx, t.Update.LatestURL)
		if err != nil {
			return err
		}
		version = tag
		if rec := m.loadInstalled().Tools[name]; rec != nil && m.versionOnDisk(name, tag, rec) {
			return m.activate(name, tag, true)
		}
		asset := path.Base(urlPath(plat.URL))
		expand := strings.NewReplacer("{version}", tag, "{asset}", asset)
		sums, err := m.getSmall(ctx, strings.NewReplacer("{version}", tag, "{asset}", t.Update.Checksums).Replace(t.Update.DownloadURL), 1<<20)
		if err != nil {
			return err
		}
		sum := findChecksum(sums, asset)
		if sum == "" {
			return app.E(app.CodeToolFailed, "%s %s 的校验文件中没有 %s", name, tag, asset).
				WithDetail("tool", name).WithDetail("version", tag)
		}
		np := plat
		np.URL, np.SHA256, np.Size = expand.Replace(t.Update.DownloadURL), sum, 0
		return m.installTool(ctx, name, toolSource{version: tag, plat: np, origin: sourceUpdate, pin: true}, report)
	})
	if err != nil {
		return "", err
	}
	return version, nil
}

// findChecksum looks up asset in a "<sha256>  <file>" list.
func findChecksum(sums []byte, asset string) string {
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset && sha256RE.MatchString(strings.ToLower(f[0])) {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

// Rollback switches a tool back to the version that was active before the
// last update (or rollback) and returns it.
func (m *Manager) Rollback(name string) (version string, err error) {
	if _, ok := m.manifest.Tools[name]; !ok {
		return "", errUnknownItem(name)
	}
	rec := m.loadInstalled().Tools[name]
	if rec == nil || rec.Previous == "" || !m.versionOnDisk(name, rec.Previous, rec) {
		return "", app.E(app.CodeNotFound, "%s 没有可回退的旧版本", name)
	}
	prev := rec.Previous
	if err := m.activate(name, prev, true); err != nil {
		return "", err
	}
	m.changed()
	return prev, nil
}
