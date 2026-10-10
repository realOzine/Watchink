package pipeline

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"

	"watchink/internal/app"
	"watchink/internal/jobs"
	"watchink/internal/media"
	"watchink/internal/store"
	"watchink/internal/tools"
)

// downloadPolicy names the yt-dlp format policy of media.Download; it is
// part of the acquire fingerprint.
const downloadPolicy = "h264-aac-1080-v1"

// sourceFile is a source media file placed in the library by acquire.
type sourceFile struct {
	rel    string // path relative to the data dir
	sha256 string
	size   int64
}

// acquire puts the source media into <data>/library/<video_id>/source.<ext>:
// a local file is copied, an upload is moved, a link is downloaded.
func (p *Pipeline) acquire(ctx context.Context, sc *jobs.StageContext) (*jobs.StageResult, error) {
	opt, err := options(sc)
	if err != nil {
		return nil, err
	}
	v, err := p.st.GetVideo(ctx, sc.VideoID())
	if err != nil {
		return nil, err
	}
	fp := jobs.Fingerprint(app.StageAcquire, acquireVersion, map[string]any{"source_key": v.SourceKey, "policy": downloadPolicy})
	if a := p.cached(ctx, sc, store.KindSource, fp, false); a != nil {
		return reused(a), nil
	}
	var src sourceFile
	switch v.SourceType {
	case app.SourceFile:
		src, err = p.copyLocalFile(ctx, sc, v, opt.Path)
	case app.SourceUpload:
		src, err = p.takeUpload(ctx, v, opt.UploadID)
	case app.SourceYouTube, app.SourceBilibili:
		src, err = p.download(ctx, sc, v)
	default:
		err = app.E(app.CodeUnsupportedSource, "不支持的来源类型：%s", v.SourceType)
	}
	if err != nil {
		return nil, err
	}
	a, err := p.st.PutArtifact(ctx, &store.Artifact{VideoID: v.ID, Kind: store.KindSource, Fingerprint: fp, Path: src.rel, SHA256: src.sha256, SizeBytes: src.size})
	if err != nil {
		return nil, err
	}
	return done(a), nil
}

// copyLocalFile copies the file into the library, hashing on the way.
func (p *Pipeline) copyLocalFile(ctx context.Context, sc *jobs.StageContext, v *store.Video, from string) (sourceFile, error) {
	if from == "" {
		return sourceFile{}, app.E(app.CodeInvalidArgument, "任务缺少本地文件路径")
	}
	dir := p.st.VideoDir(v.ID)
	rel := path.Join(dir, "source"+safeExt(from))
	if err := os.MkdirAll(p.st.Abs(dir), 0o755); err != nil {
		return sourceFile{}, err
	}
	sha, size, err := media.CopyWithSHA256(ctx, from, p.st.Abs(rel))
	if err != nil {
		if os.IsNotExist(err) {
			return sourceFile{}, app.E(app.CodeNotFound, "源文件不存在：%s", from)
		}
		return sourceFile{}, err
	}
	sc.Progress(1)
	return sourceFile{rel, sha, size}, nil
}

// takeUpload moves a pending Web upload into the library.
func (p *Pipeline) takeUpload(ctx context.Context, v *store.Video, uploadID string) (sourceFile, error) {
	up, err := p.st.GetUpload(ctx, uploadID)
	if err != nil {
		return sourceFile{}, err
	}
	rel := path.Join(p.st.VideoDir(v.ID), "source"+safeExt(up.Filename))
	fi, err := p.st.CommitFile(p.st.Abs(up.Path), rel)
	if err != nil {
		return sourceFile{}, app.E(app.CodeNotFound, "上传文件已不存在，请重新上传：%v", err)
	}
	return sourceFile{rel, fi.SHA256, fi.Size}, nil
}

// download fetches a YouTube / Bilibili video with yt-dlp and adopts the
// platform's title for the video.
func (p *Pipeline) download(ctx context.Context, sc *jobs.StageContext, v *store.Video) (sourceFile, error) {
	ytdlp, err := p.tools.Path(tools.ToolYtDlp)
	if err != nil {
		return sourceFile{}, err
	}
	deno := ""
	if v.SourceType == app.SourceYouTube {
		// yt-dlp needs an external JS runtime for YouTube only.
		if deno, err = p.tools.Path(tools.ToolDeno); err != nil {
			return sourceFile{}, err
		}
	}
	dir := p.st.VideoDir(v.ID)
	work := p.st.Abs(path.Join(dir, "download.tmp"))
	if err := os.MkdirAll(work, 0o755); err != nil {
		return sourceFile{}, err
	}
	defer os.RemoveAll(work)
	res, err := media.Download(ctx, p.runner, media.DownloadOptions{
		YtDlp: ytdlp, FFmpegDir: p.tools.BinDir(), DenoPath: deno, Proxy: p.cfg().Network.Proxy,
		URL: v.SourceURI, SourceType: v.SourceType, OutDir: work, Progress: sc.Progress,
	})
	if err != nil {
		return sourceFile{}, err
	}
	rel := path.Join(dir, "source"+safeExt(res.Path))
	fi, err := p.st.CommitFile(res.Path, rel)
	if err != nil {
		return sourceFile{}, err
	}
	if t := strings.TrimSpace(res.Title); t != "" {
		if _, err := p.st.UpdateVideo(ctx, v.ID, store.VideoPatch{Title: &t}); err == nil {
			p.changed(v.ID)
		}
	}
	return sourceFile{rel, fi.SHA256, fi.Size}, nil
}

// safeExt returns a lowercase, short, alphanumeric extension (with the dot)
// or ".bin": file names inside the library never carry user-controlled text.
func safeExt(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if len(ext) < 2 || len(ext) > 8 {
		return ".bin"
	}
	for _, c := range ext[1:] {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return ".bin"
		}
	}
	return ext
}
