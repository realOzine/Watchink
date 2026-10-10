package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"watchink/internal/app"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	// A directory with a space and non-ASCII characters, like the macOS
	// default ("Application Support").
	dir := filepath.Join(t.TempDir(), "Application Support", "数据")
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustVideo(t *testing.T, q *Queries, key, title string) *Video {
	t.Helper()
	v := &Video{SourceType: app.SourceFile, SourceKey: key, Title: title}
	if err := q.CreateVideo(context.Background(), v); err != nil {
		t.Fatalf("CreateVideo: %v", err)
	}
	return v
}

func TestOpenMigrateReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := s.UserVersion(ctx); v != SchemaVersion() || v < 1 {
		t.Fatalf("user_version = %d, want %d", v, SchemaVersion())
	}
	if err := s.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}
	for _, pragma := range []struct{ name, want string }{
		{"journal_mode", "wal"}, {"foreign_keys", "1"}, {"synchronous", "1"}, {"busy_timeout", "5000"},
	} {
		var got string
		if err := s.w.QueryRowContext(ctx, "PRAGMA "+pragma.name).Scan(&got); err != nil || got != pragma.want {
			t.Errorf("writer PRAGMA %s = %q (%v), want %q", pragma.name, got, err, pragma.want)
		}
		if err := s.r.QueryRowContext(ctx, "PRAGMA "+pragma.name).Scan(&got); err != nil || got != pragma.want {
			t.Errorf("reader PRAGMA %s = %q (%v), want %q", pragma.name, got, err, pragma.want)
		}
	}
	v := mustVideo(t, s.Queries, "file:1:abc", "hello")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(ctx, dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	got, err := s.GetVideo(ctx, v.ID)
	if err != nil || got.Title != "hello" {
		t.Fatalf("after reopen: %+v, %v", got, err)
	}
	// An up-to-date database is not backed up.
	if m, _ := filepath.Glob(filepath.Join(dir, DBFileName+".bak-*")); len(m) != 0 {
		t.Fatalf("unexpected backups: %v", m)
	}
}

func TestReadPoolIsReadOnly(t *testing.T) {
	s := openTest(t)
	if _, err := s.r.Exec(`DELETE FROM videos`); err == nil {
		t.Fatal("write through the read pool succeeded")
	}
}

func TestNewerVersionRefused(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion()+1)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	_, err = Open(ctx, dir)
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("err = %v, want ErrSchemaTooNew", err)
	}
	ae := app.AsError(err)
	if ae.Hint == "" || !ae.UserActionRequired || !strings.Contains(ae.Hint, "升级") {
		t.Fatalf("error lacks an upgrade hint: %+v", ae)
	}
}

func TestMigrateBacksUpExistingDB(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	v := mustVideo(t, s.Queries, "file:1:abc", "before")
	s.Close()

	migs, err := embeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	old := len(migs)
	migs = append(migs, migration{version: old + 1, name: "fake.sql",
		sql: `ALTER TABLE videos ADD COLUMN extra TEXT; UPDATE videos SET title = 'after';`})
	s, err = openWith(ctx, dir, migs)
	if err != nil {
		t.Fatalf("openWith: %v", err)
	}
	if ver, _ := s.UserVersion(ctx); ver != old+1 {
		t.Fatalf("user_version = %d", ver)
	}
	if got, _ := s.GetVideo(ctx, v.ID); got == nil || got.Title != "after" {
		t.Fatalf("migration not applied: %+v", got)
	}
	s.Close()

	bak := filepath.Join(dir, fmt.Sprintf("%s.bak-%d", DBFileName, old))
	if _, err := os.Stat(bak); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	// The backup is a complete pre-migration database: move it into a fresh
	// data dir and open it with the old migration set.
	dir2 := t.TempDir()
	if err := os.Rename(bak, filepath.Join(dir2, DBFileName)); err != nil {
		t.Fatal(err)
	}
	b, err := openWith(ctx, dir2, migs[:old])
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer b.Close()
	if got, err := b.GetVideo(ctx, v.ID); err != nil || got.Title != "before" {
		t.Fatalf("backup content: %+v, %v", got, err)
	}

	// A failing migration leaves version and schema untouched.
	bad := append(migs[:old+1:old+1], migration{version: old + 2, name: "bad.sql", sql: `CREATE TABLE x (a); SELECT nope FROM x;`})
	if _, err := openWith(ctx, dir, bad); err == nil {
		t.Fatal("bad migration succeeded")
	}
	s, err = openWith(ctx, dir, migs)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if ver, _ := s.UserVersion(ctx); ver != old+1 {
		t.Fatalf("user_version after failed migration = %d", ver)
	}
	if _, err := s.w.Exec(`SELECT * FROM x`); err == nil {
		t.Fatal("table from the failed migration exists")
	}
}

func TestVideosCRUDAndUniqueSourceKey(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	v := mustVideo(t, s.Queries, "youtube:abcdefghijk", "Lecture")
	if !strings.HasPrefix(v.ID, app.PrefixVideo) || v.CreatedAt == "" {
		t.Fatalf("defaults not filled: %+v", v)
	}
	err := s.CreateVideo(ctx, &Video{SourceType: app.SourceYouTube, SourceKey: "youtube:abcdefghijk", Title: "dup"})
	if !IsUniqueViolation(err) || !app.IsCode(err, app.CodeConflict) {
		t.Fatalf("duplicate source_key: %v", err)
	}
	if got, err := s.FindVideoBySourceKey(ctx, "nope"); got != nil || err != nil {
		t.Fatalf("FindVideoBySourceKey(missing) = %v, %v", got, err)
	}
	if _, err := s.GetVideo(ctx, "vid_missing"); !app.IsCode(err, app.CodeNotFound) {
		t.Fatalf("GetVideo(missing) = %v", err)
	}
	dur, title, lang, tr := int64(1234), "New", "zh", "tr_x"
	got, err := s.UpdateVideo(ctx, v.ID, VideoPatch{Title: &title, DurationMs: &dur, Language: &lang, CurrentTranscriptID: &tr})
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "New" || got.DurationMs == nil || *got.DurationMs != 1234 || got.Language != "zh" ||
		got.CurrentTranscriptID != "tr_x" || got.CurrentNoteID != "" {
		t.Fatalf("UpdateVideo: %+v", got)
	}
	empty := ""
	if got, _ = s.UpdateVideo(ctx, v.ID, VideoPatch{CurrentTranscriptID: &empty}); got.CurrentTranscriptID != "" || got.Title != "New" {
		t.Fatalf("clearing a nullable column: %+v", got)
	}
	if _, err := s.UpdateVideo(ctx, "vid_missing", VideoPatch{Title: &title}); !app.IsCode(err, app.CodeNotFound) {
		t.Fatalf("UpdateVideo(missing) = %v", err)
	}
}

func TestListVideosPagination(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	var ids []string
	for i := range 25 {
		title := fmt.Sprintf("video %02d", i)
		if i%5 == 0 {
			title = fmt.Sprintf("100%%_special %02d", i)
		}
		ids = append(ids, mustVideo(t, s.Queries, fmt.Sprintf("file:%d:x", i), title).ID)
	}
	var seen []string
	cursor := ""
	pages := 0
	for {
		vs, next, err := s.ListVideos(ctx, VideoFilter{Limit: 10, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, v := range vs {
			seen = append(seen, v.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if pages != 3 || len(seen) != 25 {
		t.Fatalf("pages = %d, rows = %d", pages, len(seen))
	}
	for i, id := range seen { // newest first, no duplicates, no gaps
		if id != ids[len(ids)-1-i] {
			t.Fatalf("row %d = %s, want %s", i, id, ids[len(ids)-1-i])
		}
	}
	// Exactly one full page has no next cursor.
	if vs, next, _ := s.ListVideos(ctx, VideoFilter{Limit: 25}); len(vs) != 25 || next != "" {
		t.Fatalf("full page: %d rows, next %q", len(vs), next)
	}
	// LIKE wildcards in the filter are literals.
	vs, next, err := s.ListVideos(ctx, VideoFilter{TitleContains: "100%_SPECIAL", Limit: 3})
	if err != nil || len(vs) != 3 || next == "" {
		t.Fatalf("filtered page: %d rows, next %q, %v", len(vs), next, err)
	}
	vs2, next2, _ := s.ListVideos(ctx, VideoFilter{TitleContains: "100%_SPECIAL", Limit: 3, Cursor: next})
	if len(vs2) != 2 || next2 != "" {
		t.Fatalf("filtered page 2: %d rows, next %q", len(vs2), next2)
	}
	if vs, _, _ := s.ListVideos(ctx, VideoFilter{TitleContains: "%"}); len(vs) != 5 {
		t.Fatalf("literal %% matched %d rows", len(vs))
	}
	if _, _, err := s.ListVideos(ctx, VideoFilter{Cursor: "!!"}); !app.IsCode(err, app.CodeInvalidArgument) {
		t.Fatalf("bad cursor: %v", err)
	}
	if vs, _, _ := s.ListVideos(ctx, VideoFilter{Limit: 1000}); len(vs) != 25 {
		t.Fatalf("limit clamp: %d", len(vs))
	}
}

// importOnce is the §7.5 flow reduced to its transactional core.
func importOnce(ctx context.Context, s *Store, key string) (videoID, jobID string, created bool, err error) {
	err = s.Write(ctx, func(tx *Tx) error {
		v, err := tx.FindVideoBySourceKey(ctx, key)
		if err != nil {
			return err
		}
		if v == nil {
			v = &Video{SourceType: app.SourceFile, SourceKey: key, Title: "t"}
			if err := tx.CreateVideo(ctx, v); err != nil {
				return err
			}
		}
		videoID = v.ID
		active, err := tx.ActiveJobOfVideo(ctx, v.ID)
		if err != nil {
			return err
		}
		if active != nil {
			jobID = active.ID
			return nil
		}
		j, err := tx.CreateJob(ctx, &Job{Kind: app.JobKindProcessVideo, VideoID: v.ID}, []string{"a", "b"})
		if err != nil {
			return err
		}
		jobID, created = j.ID, true
		return nil
	})
	return
}

func TestConcurrentImportsYieldOneVideoOneJob(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	const n = 32
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		videos  = map[string]int{}
		jobs    = map[string]int{}
		creates int
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			vid, job, created, err := importOnce(ctx, s, "file:42:same")
			if err != nil {
				t.Errorf("import: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			videos[vid]++
			jobs[job]++
			if created {
				creates++
			}
		}()
	}
	wg.Wait()
	if len(videos) != 1 || len(jobs) != 1 || creates != 1 {
		t.Fatalf("videos=%d jobs=%d creates=%d, want 1/1/1", len(videos), len(jobs), creates)
	}
	if vs, _, _ := s.ListVideos(ctx, VideoFilter{}); len(vs) != 1 {
		t.Fatalf("%d video rows", len(vs))
	}
	if c, _ := s.CountActiveJobs(ctx); c != 1 {
		t.Fatalf("%d active jobs", c)
	}
}

func TestWriteRollsBack(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	boom := errors.New("boom")
	err := s.Write(ctx, func(tx *Tx) error {
		mustVideo(t, tx.Queries, "file:1:a", "x")
		if v, _ := tx.FindVideoBySourceKey(ctx, "file:1:a"); v == nil {
			t.Error("tx does not see its own write")
		}
		// The pool must not see uncommitted data.
		if v, _ := s.FindVideoBySourceKey(ctx, "file:1:a"); v != nil {
			t.Error("uncommitted row visible outside the tx")
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if v, _ := s.FindVideoBySourceKey(ctx, "file:1:a"); v != nil {
		t.Fatal("rolled-back row exists")
	}
	func() {
		defer func() { recover() }()
		s.Write(ctx, func(tx *Tx) error {
			mustVideo(t, tx.Queries, "file:1:b", "x")
			panic("p")
		})
	}()
	// The writer connection is free again and the panic rolled back.
	if v, _ := s.FindVideoBySourceKey(ctx, "file:1:b"); v != nil {
		t.Fatal("row from a panicked tx exists")
	}
	mustVideo(t, s.Queries, "file:1:c", "x")
}

func TestArtifacts(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	v := mustVideo(t, s.Queries, "file:1:a", "x")

	rel := s.VideoDir(v.ID) + "/work/fp1.json"
	fi, err := s.WriteFileAtomic(rel, []byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size != 7 || len(fi.SHA256) != 64 {
		t.Fatalf("FileInfo = %+v", fi)
	}
	a, err := s.PutArtifact(ctx, &Artifact{VideoID: v.ID, Kind: KindMapChunk, Fingerprint: "fp1", Path: rel, SHA256: fi.SHA256, SizeBytes: fi.Size})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.ID, app.PrefixArtifact) || string(a.Meta) != "{}" || a.Purged {
		t.Fatalf("artifact = %+v", a)
	}
	if !s.VerifyArtifact(a) || !s.ArtifactPresent(a) {
		t.Fatal("fresh artifact does not verify")
	}
	if got, _ := s.FindArtifact(ctx, KindMapChunk, "fp1"); got == nil || got.ID != a.ID {
		t.Fatalf("FindArtifact = %+v", got)
	}
	if got, err := s.FindArtifact(ctx, KindMapChunk, "other"); got != nil || err != nil {
		t.Fatalf("FindArtifact(missing) = %v, %v", got, err)
	}

	// Same size, different content: the cheap check passes, the hash fails.
	if err := os.WriteFile(s.Abs(rel), []byte(`{"a":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !s.ArtifactPresent(a) || s.VerifyArtifact(a) {
		t.Fatal("corrupted file not detected by VerifyArtifact")
	}

	// Upsert: same (kind, fingerprint) keeps the ID, replaces the content and clears purged.
	if err := s.PurgeArtifact(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	p, _ := s.GetArtifact(ctx, a.ID)
	if !p.Purged || s.VerifyArtifact(p) {
		t.Fatalf("purged artifact still reusable: %+v", p)
	}
	if _, err := os.Stat(s.Abs(rel)); !os.IsNotExist(err) {
		t.Fatalf("purged file still there: %v", err)
	}
	fi2, _ := s.WriteFileAtomic(rel, []byte(`{"a":333}`))
	a2, err := s.PutArtifact(ctx, &Artifact{VideoID: v.ID, Kind: KindMapChunk, Fingerprint: "fp1", Path: rel,
		SHA256: fi2.SHA256, SizeBytes: fi2.Size, Meta: []byte(`{"n":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if a2.ID != a.ID || a2.Purged || a2.SHA256 != fi2.SHA256 || string(a2.Meta) != `{"n":1}` || !s.VerifyArtifact(a2) {
		t.Fatalf("upsert: %+v", a2)
	}

	// Metadata-only artifacts (no file) verify by row alone; a different kind
	// may reuse the fingerprint.
	m, err := s.PutArtifact(ctx, &Artifact{VideoID: v.ID, Kind: KindProbe, Fingerprint: "fp1"})
	if err != nil || m.ID == a.ID || !s.VerifyArtifact(m) {
		t.Fatalf("metadata artifact: %+v, %v", m, err)
	}
	if all, _ := s.ListArtifacts(ctx, v.ID, ""); len(all) != 2 {
		t.Fatalf("ListArtifacts = %d", len(all))
	}
	if only, _ := s.ListArtifacts(ctx, v.ID, KindProbe); len(only) != 1 {
		t.Fatalf("ListArtifacts(kind) = %d", len(only))
	}
	if s.VerifyArtifact(nil) {
		t.Fatal("nil artifact verified")
	}
	if err := s.MarkArtifactPurged(ctx, "art_missing"); !app.IsCode(err, app.CodeNotFound) {
		t.Fatalf("MarkArtifactPurged(missing) = %v", err)
	}
	// Foreign key: an artifact needs an existing video.
	if _, err := s.PutArtifact(ctx, &Artifact{VideoID: "vid_missing", Kind: KindProbe, Fingerprint: "z"}); err == nil {
		t.Fatal("artifact without a video accepted")
	}
}

func TestFileHelpers(t *testing.T) {
	s := openTest(t)
	abs := s.Abs("library/vid_1/note.md")
	if rel, err := s.Rel(abs); err != nil || rel != "library/vid_1/note.md" {
		t.Fatalf("Rel = %q, %v", rel, err)
	}
	if got := s.Abs("../../etc/passwd"); !strings.HasPrefix(got, s.DataDir()+string(filepath.Separator)) {
		t.Fatalf("Abs escaped the data dir: %s", got)
	}
	if _, err := s.Rel(filepath.Dir(s.DataDir())); err == nil {
		t.Fatal("Rel accepted a path outside the data dir")
	}
	if _, err := s.Rel(s.DataDir() + "-sibling/x"); err == nil {
		t.Fatal("Rel accepted a sibling directory")
	}

	tmp, err := s.TempPath("exports/vid_1/out.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := s.CommitFile(tmp, "exports/vid_1/out.md")
	if err != nil {
		t.Fatal(err)
	}
	// sha256("hello")
	if fi.SHA256 != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" || fi.Size != 5 {
		t.Fatalf("FileInfo = %+v", fi)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
	if got, err := SHA256File(s.Abs("exports/vid_1/out.md")); err != nil || got != fi {
		t.Fatalf("SHA256File = %+v, %v", got, err)
	}
	if _, err := s.CommitFile(filepath.Join(s.DataDir(), "missing.tmp"), "x"); err == nil {
		t.Fatal("CommitFile of a missing temp file succeeded")
	}
	if err := s.RemoveFile("exports/vid_1/out.md"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveFile("exports/vid_1/out.md"); err != nil {
		t.Fatalf("removing a missing file: %v", err)
	}
}

func TestSweepOrphans(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	v := mustVideo(t, s.Queries, "file:1:a", "x")
	write := func(rel string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(s.Abs(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.Abs(rel), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(rel string) bool {
		_, err := os.Stat(s.Abs(rel))
		return err == nil
	}
	keep := []string{
		s.VideoDir(v.ID) + "/source.mp4", s.VideoDir(v.ID) + "/notes/note_1/note.md",
		"uploads/upl_live.part", "uploads/upl_consumed.part", "tools/x/1/download.part", "exports/" + v.ID + "/n.md",
	}
	gone := []string{
		s.VideoDir(v.ID) + "/audio.wav.abc.tmp", s.VideoDir(v.ID) + "/work/dl.part", "exports/" + v.ID + "/n.md.1.tmp",
		"library/vid_ORPHAN/source.mp4", "uploads/upl_norow.part", "uploads/upl_expired.part", "uploads/x.tmp",
	}
	for _, f := range append(append([]string{}, keep...), gone...) {
		write(f)
	}
	past, future := app.FormatTime(time.Now().Add(-time.Hour)), app.FormatTime(time.Now().Add(time.Hour))
	for _, u := range []*Upload{
		{ID: "upl_live", Filename: "a", Path: "uploads/upl_live.part", ExpiresAt: future, SampleHash: "h"},
		{ID: "upl_expired", Filename: "a", Path: "uploads/upl_expired.part", ExpiresAt: past, SampleHash: "h"},
		{ID: "upl_consumed", Filename: "a", Path: "uploads/upl_consumed.part", ExpiresAt: past, SampleHash: "h", ConsumedBy: v.ID},
	} {
		if err := s.CreateUpload(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := s.SweepOrphans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range keep {
		if !exists(f) {
			t.Errorf("sweep removed %s", f)
		}
	}
	for _, f := range gone {
		if exists(f) {
			t.Errorf("sweep kept %s", f)
		}
	}
	if len(rep.ExpiredUploads) != 1 || rep.ExpiredUploads[0] != "upl_expired" || len(rep.VideoDirs) != 1 || len(rep.TempFiles) != 5 {
		t.Errorf("report = %+v", rep)
	}
	if _, err := s.GetUpload(ctx, "upl_expired"); !app.IsCode(err, app.CodeNotFound) {
		t.Errorf("expired upload row kept: %v", err)
	}
	if _, err := s.GetUpload(ctx, "upl_consumed"); err != nil {
		t.Errorf("consumed upload row removed: %v", err)
	}
	// Sweeping an empty data dir is fine.
	if _, err := openTest(t).SweepOrphans(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestUploadsExportsChildProcesses(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	u := &Upload{Filename: "a.mp4", SizeBytes: 9, SampleHash: "h", Path: "uploads/x.part", ExpiresAt: app.Now()}
	if err := s.CreateUpload(ctx, u); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetUpload(ctx, u.ID); err != nil || got.Filename != "a.mp4" || got.ConsumedBy != "" || got.DTO().UploadID != u.ID {
		t.Fatalf("GetUpload = %+v, %v", got, err)
	}
	if got, err := s.ConsumeUpload(ctx, u.ID, "vid_1"); err != nil || got.ConsumedBy != "vid_1" {
		t.Fatalf("ConsumeUpload = %+v, %v", got, err)
	}
	if _, err := s.ConsumeUpload(ctx, u.ID, "vid_1"); err != nil {
		t.Fatalf("idempotent consume: %v", err)
	}
	if _, err := s.ConsumeUpload(ctx, u.ID, "vid_2"); !app.IsCode(err, app.CodeConflict) {
		t.Fatalf("second consumer: %v", err)
	}
	if _, err := s.ConsumeUpload(ctx, "upl_missing", "vid_1"); !app.IsCode(err, app.CodeNotFound) {
		t.Fatalf("consume missing: %v", err)
	}
	if exp, _ := s.ListExpiredUploads(ctx, time.Now().Add(time.Hour)); len(exp) != 0 {
		t.Fatalf("consumed upload listed as expired")
	}
	if err := s.DeleteUpload(ctx, u.ID); err != nil {
		t.Fatal(err)
	}

	e := &Export{VideoID: "vid_1", Format: app.FormatMD, Path: "exports/vid_1/a.md", SHA256: "s"}
	if err := s.CreateExport(ctx, e); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetExport(ctx, e.ID); err != nil || got.Path != e.Path || got.NoteID != "" {
		t.Fatalf("GetExport = %+v, %v", got, err)
	}
	if _, err := s.GetExport(ctx, "exp_missing"); !app.IsCode(err, app.CodeNotFound) {
		t.Fatal(err)
	}

	for _, p := range []ChildProcess{{PID: 10, JobID: "job_1", Stage: "transcribe", Exe: "/w"}, {PID: 11, Exe: "/f"}} {
		if err := s.AddChildProcess(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	ps, _ := s.ListChildProcesses(ctx)
	if len(ps) != 2 || ps[0].PID != 10 || ps[0].Stage != "transcribe" || ps[1].JobID != "" || ps[0].StartedAt == "" {
		t.Fatalf("ListChildProcesses = %+v", ps)
	}
	s.RemoveChildProcess(ctx, 10)
	if ps, _ = s.ListChildProcesses(ctx); len(ps) != 1 || ps[0].PID != 11 {
		t.Fatalf("after remove: %+v", ps)
	}
	s.ClearChildProcesses(ctx)
	if ps, _ = s.ListChildProcesses(ctx); len(ps) != 0 {
		t.Fatalf("after clear: %+v", ps)
	}
}
