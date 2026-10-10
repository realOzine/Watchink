package tools

import (
	"context"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"watchink/internal/app"
)

// Sources recorded in installed.json.
const (
	sourceDownload = "download"
	sourceImport   = "import"
	sourceEmbedded = "embedded"
	sourceUpdate   = "update"
)

// toolSource says where the bytes of one tool version come from.
type toolSource struct {
	version string
	plat    Platform // archive kind, files, expected sha256, URL
	origin  string
	// localFile is the user's file for an import; it is copied, never moved.
	localFile string
	// pin marks the installed version as the user's choice.
	pin bool
}

func noReport(done, total int64) {}

// ensureTool installs the wanted version of a tool unless it is ready.
func (m *Manager) ensureTool(ctx context.Context, name string, report func(done, total int64)) error {
	t := m.manifest.Tools[name]
	inst := m.loadInstalled()
	info := m.toolInfo(inst, name)
	if info.problem != nil {
		return info.problem
	}
	if info.ready {
		return nil
	}
	if t.Embedded {
		sum, _ := m.embeddedSHA(name)
		bin := exeName(m.platform, name)
		return m.installTool(ctx, name, toolSource{
			version: t.Version,
			origin:  sourceEmbedded,
			plat:    Platform{SHA256: sum, Archive: ArchiveNone, Files: map[string]string{bin: bin}, Bin: bin},
		}, report)
	}
	plat := t.Platforms[m.platform]
	// The manifest version may already be on disk while another one is
	// active (an unpinned record after an app upgrade): just switch to it.
	if rec := inst.Tools[name]; rec != nil && m.versionOnDisk(name, t.Version, rec) {
		return m.activate(name, t.Version, false)
	}
	return m.installTool(ctx, name, toolSource{version: t.Version, plat: plat, origin: sourceDownload}, report)
}

func (m *Manager) versionOnDisk(name, version string, rec *installedTool) bool {
	v := rec.Versions[version]
	if v == nil {
		return false
	}
	st, err := os.Stat(filepath.Join(m.versionDir(name, version), v.Bin))
	return err == nil && st.Size() == v.BinSize
}

// installTool runs the flow for one tool version. Everything happens in
// a private staging directory until the final rename, so a failure at any
// step leaves an installed version untouched, and the smoke test only ever
// runs a file whose source passed the sha256 check.
func (m *Manager) installTool(ctx context.Context, name string, src toolSource, report func(done, total int64)) error {
	if report == nil {
		report = noReport
	}
	work, err := os.MkdirTemp(m.stagingDir(), name+"-*")
	if err != nil {
		return ioErr(err, "创建暂存目录")
	}
	defer os.RemoveAll(work)

	// 1–2. Obtain the payload in staging and verify its sha256.
	payload, release, err := m.obtainPayload(ctx, name, src, work, report)
	if err != nil {
		return err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}

	// 3–4. Extract and mark executable.
	out := filepath.Join(work, "out")
	bin, files, err := extractPayload(name, src, payload, out)
	if err != nil {
		return err
	}

	// 5. Smoke test, still in staging: a program that cannot run is never
	// installed.
	if err := m.smokeTest(ctx, name, src.version, filepath.Join(out, bin)); err != nil {
		return err
	}

	// 6. Move into place.
	final := m.versionDir(name, src.version)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return ioErr(err, "创建工具目录")
	}
	if err := replaceDir(out, final, filepath.Join(work, "old")); err != nil {
		return ioErr(err, "安装 "+name)
	}

	// 7. Record it and point <tools>/bin at it.
	if err := m.recordInstall(name, src, bin, files); err != nil {
		return err
	}
	m.linkBin(name, src.version, bin)
	m.log.Info("tools: installed", "tool", name, "version", src.version, "source", src.origin)
	return nil
}

// obtainPayload puts the verified payload of a tool in staging and returns
// its path: the embedded program, a file the user imported, or the pinned
// download. release must be called once the payload is no longer needed.
func (m *Manager) obtainPayload(ctx context.Context, name string, src toolSource, work string, report func(done, total int64)) (payload string, release func(), err error) {
	release = func() {}
	payload = filepath.Join(work, "payload")
	switch src.origin {
	case sourceEmbedded:
		f, err := m.embedded.Open(m.embeddedFile(name))
		if err != nil {
			if _, missing := m.embeddedSHA(name); missing != nil {
				return "", release, missing
			}
			return "", release, ioErr(err, "读取内嵌程序")
		}
		defer f.Close()
		return payload, release, m.stageVerified(f, payload, src.plat.SHA256, name)
	case sourceImport:
		f, err := os.Open(src.localFile)
		if err != nil {
			return "", release, app.E(app.CodeInvalidArgument, "无法读取要导入的文件：%v", err).WithCause(err)
		}
		defer f.Close()
		return payload, release, m.stageVerified(f, payload, src.plat.SHA256, filepath.Base(src.localFile))
	default:
		payload = m.partPath(name, src.plat.SHA256)
		if err := m.download(ctx, src.plat.URL, src.plat.SHA256, src.plat.Size, payload, report); err != nil {
			return "", release, err
		}
		// Removed, or handed to another tool that needs the same download.
		return payload, func() { m.releasePayload(name, src.plat.SHA256, payload) }, nil
	}
}

// extractPayload unpacks the files the manifest lists into out and returns
// the name of the tool's executable among them.
func extractPayload(name string, src toolSource, payload, out string) (bin string, files map[string]extracted, err error) {
	if err := os.Mkdir(out, 0o755); err != nil {
		return "", nil, ioErr(err, "创建暂存目录")
	}
	files, err = extractFiles(payload, src.plat.Archive, src.plat.Files, out)
	if err != nil {
		if e := ioErr(err, "解压 "+name); app.IsCode(e, app.CodeDiskFull) {
			return "", nil, e
		}
		return "", nil, app.E(app.CodeToolFailed, "解压 %s 失败：%v", name, err).WithCause(err).WithDetail("tool", name)
	}
	bin = src.plat.Bin
	if bin == "" {
		bin = name
	}
	if _, ok := files[bin]; !ok {
		return "", nil, app.E(app.CodeToolFailed, "%s 的安装包中没有可执行文件 %s", name, bin).WithDetail("tool", name)
	}
	return bin, files, nil
}

// smokeTest runs the staged program once (by default with --version).
func (m *Manager) smokeTest(ctx context.Context, name, version, exe string) error {
	args := m.manifest.Tools[name].SmokeArgs
	if len(args) == 0 {
		args = []string{"--version"}
	}
	sctx, cancel := context.WithTimeout(ctx, m.smokeTimeout)
	output, err := m.exec(sctx, exe, args...)
	cancel()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return app.E(app.CodeToolFailed, "%s 无法运行（自检失败）：%v", name, err).WithCause(err).
		WithHint("下载的程序可能与当前系统不兼容，或被安全软件拦截。").
		WithDetail("tool", name).WithDetail("version", version).WithDetail("output", tail(output, 2000))
}

// recordInstall writes the installed version to installed.json and makes it
// the active one.
func (m *Manager) recordInstall(name string, src toolSource, bin string, files map[string]extracted) error {
	var total int64
	for _, f := range files {
		total += f.size
	}
	binFile := files[bin]
	return m.updateInstalled(func(f *installedFile) {
		rec := f.Tools[name]
		if rec == nil {
			rec = &installedTool{Versions: map[string]*installedVersion{}}
			f.Tools[name] = rec
		}
		rec.Versions[src.version] = &installedVersion{
			SHA256: src.plat.SHA256, Bin: bin, BinSHA256: binFile.sha256, BinSize: binFile.size,
			Size: total, Source: src.origin, InstalledAt: time.Now().UTC().Truncate(time.Second),
		}
		setActive(rec, src.version, src.pin)
	})
}

// setActive switches the active version, remembering the one it replaces.
func setActive(rec *installedTool, version string, pin bool) {
	if rec.Active != version {
		if rec.Active != "" && rec.Versions[rec.Active] != nil {
			rec.Previous = rec.Active
		}
		rec.Active = version
	}
	rec.Pinned = pin
}

// activate makes an already installed version the active one.
func (m *Manager) activate(name, version string, pin bool) error {
	var bin string
	err := m.updateInstalled(func(f *installedFile) {
		if rec := f.Tools[name]; rec != nil && rec.Versions[version] != nil {
			bin = rec.Versions[version].Bin
			setActive(rec, version, pin)
		}
	})
	if err == nil && bin != "" {
		m.linkBin(name, version, bin)
	}
	return err
}

// stageVerified copies r to dst and fails with CHECKSUM_MISMATCH, deleting
// the copy, unless its sha256 is want.
func (m *Manager) stageVerified(r io.Reader, dst, want, label string) error {
	got, _, err := writeNewFile(dst, r, 0o600)
	if err != nil {
		return ioErr(err, "写入暂存文件")
	}
	if got != want {
		os.Remove(dst)
		return app.E(app.CodeChecksumMismatch, "文件校验失败：%s", label).
			WithHint("该文件与清单记录的 sha256 不一致，未安装。请确认下载的是清单指定的版本。").
			WithDetail("want_sha256", want).WithDetail("got_sha256", got)
	}
	return nil
}

func (m *Manager) partPath(name, sum string) string {
	return filepath.Join(m.partDir(), name+"-"+sum[:16]+".part")
}

// releasePayload disposes of a tool's verified download once it is no longer
// needed. Some upstreams ship several tools in one archive (the FFmpeg builds
// for Windows hold both ffmpeg and ffprobe): instead of deleting the file it
// is moved to where the next such tool, if it still has to be installed,
// would download it to, and that download then finds it complete. The other
// tool verifies the sha256 again, as it does for any resumed download.
func (m *Manager) releasePayload(name, sum, payload string) {
	inst := m.loadInstalled()
	for _, other := range m.manifest.toolNames() {
		if other == name || m.manifest.Tools[other].Platforms[m.platform].SHA256 != sum || m.toolInfo(inst, other).ready {
			continue
		}
		next := m.partPath(other, sum)
		if _, err := os.Lstat(next); err == nil {
			continue // that tool already has a download of its own under way
		}
		if os.Rename(payload, next) == nil {
			return
		}
	}
	os.Remove(payload)
}

// replaceDir renames src to dst. An existing dst is first moved to trash and
// put back if the second rename fails, so dst is never left missing.
func replaceDir(src, dst, trash string) error {
	if _, err := os.Lstat(dst); err == nil {
		if err := rename(dst, trash); err != nil {
			return err
		}
		if err := rename(src, dst); err != nil {
			rename(trash, dst)
			return err
		}
		os.RemoveAll(trash)
		return nil
	}
	return rename(src, dst)
}

// linkBin points <tools>/bin/<bin> at the active version (see BinDir). It is
// a convenience; failures are logged and do not fail the install.
func (m *Manager) linkBin(name, version, bin string) {
	if err := os.MkdirAll(m.binDir(), 0o755); err != nil {
		m.log.Warn("tools: bin dir", "err", err)
		return
	}
	link := filepath.Join(m.binDir(), bin)
	tmp := link + ".tmp"
	os.Remove(tmp)
	target := filepath.Join(m.versionDir(name, version), bin)
	if err := makeLink(target, path.Join("..", name, version, bin), tmp); err != nil {
		m.log.Warn("tools: link", "tool", name, "err", err)
		return
	}
	if err := rename(tmp, link); err != nil {
		os.Remove(tmp)
		m.log.Warn("tools: link", "tool", name, "err", err)
	}
}

// ensureModel downloads a model unless it is ready.
func (m *Manager) ensureModel(ctx context.Context, name string, report func(done, total int64)) error {
	if _, _, _, ready := m.modelInfo(m.loadInstalled(), name); ready {
		return nil
	}
	return m.installModel(ctx, name, "", report)
}

// installModel downloads (or, with localFile, imports) a model: staging,
// sha256 check, atomic rename, installed.json. Models are data, so there is
// no smoke test.
func (m *Manager) installModel(ctx context.Context, name, localFile string, report func(done, total int64)) error {
	if report == nil {
		report = noReport
	}
	md := m.manifest.Models[name]
	origin := sourceDownload
	var payload string
	if localFile != "" {
		origin = sourceImport
		work, err := os.MkdirTemp(m.stagingDir(), name+"-*")
		if err != nil {
			return ioErr(err, "创建暂存目录")
		}
		defer os.RemoveAll(work)
		f, err := os.Open(localFile)
		if err != nil {
			return app.E(app.CodeInvalidArgument, "无法读取要导入的文件：%v", err).WithCause(err)
		}
		payload = filepath.Join(work, "payload")
		err = m.stageVerified(f, payload, md.SHA256, filepath.Base(localFile))
		f.Close()
		if err != nil {
			return err
		}
	} else {
		payload = m.partPath(name, md.SHA256)
		if err := m.download(ctx, md.URL, md.SHA256, md.Size, payload, report); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	st, err := os.Stat(payload)
	if err != nil {
		return ioErr(err, "安装模型")
	}
	dir := filepath.Join(m.modelsDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ioErr(err, "创建模型目录")
	}
	if err := os.Chmod(payload, 0o644); err != nil {
		return ioErr(err, "安装模型")
	}
	if err := rename(payload, filepath.Join(dir, md.FileName())); err != nil {
		return ioErr(err, "安装模型")
	}
	err = m.updateInstalled(func(f *installedFile) {
		f.Models[name] = &installedModel{
			File: md.FileName(), SHA256: md.SHA256, Size: st.Size(), Source: origin,
			InstalledAt: time.Now().UTC().Truncate(time.Second),
		}
	})
	if err != nil {
		return err
	}
	m.log.Info("tools: installed model", "model", name, "source", origin)
	return nil
}

// Import installs a tool or model from a file the user downloaded by hand.
// The file must be exactly the download the manifest pins: its
// sha256 is checked before anything is extracted or run. The file itself is
// left untouched.
func (m *Manager) Import(ctx context.Context, name, file string) error {
	t, isTool := m.manifest.Tools[name]
	_, isModel := m.manifest.Models[name]
	switch {
	case !isTool && !isModel:
		return errUnknownItem(name)
	case isTool && t.Embedded:
		return app.E(app.CodeInvalidArgument, "%s 随程序内嵌，不支持导入", name).
			WithHint("开发时可用环境变量 " + EnvWhisperBin + " 指定外部可执行文件。")
	}
	if st, err := os.Stat(file); err != nil || st.IsDir() {
		return app.E(app.CodeInvalidArgument, "要导入的文件不存在：%s", file)
	}
	return m.exclusive(ctx, name, false, nil, func(func(done, total int64)) error {
		if isModel {
			return m.installModel(ctx, name, file, nil)
		}
		plat, ok := t.Platforms[m.platform]
		if !ok {
			return m.errNoPlatform(name)
		}
		return m.installTool(ctx, name, toolSource{version: t.Version, plat: plat, origin: sourceImport, localFile: file}, nil)
	})
}

func tail(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return strings.ToValidUTF8(s, "?")
}
