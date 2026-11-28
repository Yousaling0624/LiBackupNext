package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/restic/restic/internal/restic"
)

func strptr(value string) *string { return &value }

type frameSink struct {
	mu     sync.Mutex
	frames []string
}

func (s *frameSink) write(frame string) {
	s.mu.Lock()
	s.frames = append(s.frames, frame)
	s.mu.Unlock()
}

func (s *frameSink) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.frames...)
}

func (s *frameSink) has(part string) bool {
	for _, frame := range s.snapshot() {
		if strings.Contains(frame, part) {
			return true
		}
	}
	return false
}

func setupRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	if err := InitRepo(&InitRequest{Path: repoDir, Password: "test-password", ForceInit: true}); err != nil {
		t.Fatalf("init repo: %v", err)
	}
	SetCacheDir("")
	SetRepo(repoDir, "test-password", nil)
	t.Cleanup(func() {
		SetRepo("", "", nil)
		closeEverything()
	})
	return repoDir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBackupListOverviewRestore(t *testing.T) {
	setupRepo(t)

	src := t.TempDir()
	writeFile(t, filepath.Join(src, "file.txt"), "hello restic")
	writeFile(t, filepath.Join(src, "dir", "data.bin"), strings.Repeat("x", 4096))
	writeFile(t, filepath.Join(src, ".thumbnails", "thumb.jpg"), "thumb")
	writeFile(t, filepath.Join(src, ".trashed-123"), "trash")
	if err := os.Symlink("file.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(src, "file.txt"), filepath.Join(src, "dir", "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "file.txt"), 0o640); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1_700_000_000, 123_000_000)
	if err := os.Chtimes(filepath.Join(src, "file.txt"), mtime, mtime); err != nil {
		t.Fatal(err)
	}

	sink := &frameSink{}
	request := &BackupRequest{
		Items: []BackupItem{{
			Stage:       "user",
			Label:       "com.test:user",
			Tags:        []string{"TestApp|1.0|owner|1700000000000"},
			Sources:     []string{src},
			AsPath:      strptr("com.test"),
			Description: strptr("first note"),
			Excludes:    []string{"!.thumbnails", "!.trashed*"},
		}},
	}
	progress := NewProgress(sink.write)
	if err := Backup(context.Background(), request, progress); err != nil {
		t.Fatalf("backup: %v", err)
	}
	progress.Finish() // JNI 包装层在 Backup 返回后写
	if !sink.has(`{"stage":"user","state":"running"}`) {
		t.Fatalf("missing running frame: %v", sink.snapshot())
	}
	if !sink.has(`"state":"done"`) || !sink.has(`"files":`) {
		t.Fatalf("missing done frame: %v", sink.snapshot())
	}
	if !sink.has(`{"stage":"","state":"finished"}`) {
		t.Fatalf("missing finished frame: %v", sink.snapshot())
	}

	snapshots, err := ListSnapshots("com.test")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("want 1 snapshot, got %d", len(snapshots))
	}
	info := snapshots[0]
	if info.Label != "com.test:user" || info.Tags != "TestApp|1.0|owner|1700000000000" {
		t.Fatalf("unexpected snapshot info: %+v", info)
	}
	if info.Description == nil || *info.Description != "first note" {
		t.Fatalf("description not preserved: %+v", info)
	}
	if info.Size == 0 {
		t.Fatalf("snapshot size is zero")
	}

	packages, err := Overview()
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if len(packages) != 1 || packages[0].Package != "com.test" || packages[0].Count != 1 {
		t.Fatalf("unexpected overview: %+v", packages)
	}
	if packages[0].Time != 1_700_000_000_000 {
		t.Fatalf("overview time %d", packages[0].Time)
	}
	if packages[0].Description == nil || *packages[0].Description != "first note" {
		t.Fatalf("overview description: %+v", packages[0])
	}

	// 自定义快照应保留 rustic 字段：label / paths / description / summary。
	repo, err := openRepo()
	if err != nil {
		t.Fatal(err)
	}
	defer repo.release()
	records, err := loadAllSnapshots(context.Background(), repo.Repository)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("raw snapshots: %d", len(records))
	}
	rec := records[0]
	if rec.Label != "com.test:user" || len(rec.Paths) != 1 || rec.Paths[0] != "com.test" {
		t.Fatalf("raw label/paths: %+v", rec)
	}
	if summary, ok := rec.Raw["summary"].(map[string]any); !ok || summary["total_files_processed"] == nil {
		t.Fatalf("raw summary missing: %+v", rec.Raw)
	}

	dest := t.TempDir()
	writeFile(t, filepath.Join(dest, "com.test", "extra", "old.txt"), "remove me")
	writeFile(t, filepath.Join(dest, "keep.txt"), "keep me")

	restoreSink := &frameSink{}
	err = Restore(context.Background(), &RestoreRequest{Items: []RestoreItem{{
		ID: info.ID, Dest: dest, Delete: true, Stage: "user",
	}}}, NewProgress(restoreSink.write))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	restored := filepath.Join(dest, "com.test")
	data, err := os.ReadFile(filepath.Join(restored, "file.txt"))
	if err != nil || string(data) != "hello restic" {
		t.Fatalf("restored file: %q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(restored, ".thumbnails")); !os.IsNotExist(err) {
		t.Fatalf(".thumbnails should be excluded")
	}
	if _, err := os.Stat(filepath.Join(restored, ".trashed-123")); !os.IsNotExist(err) {
		t.Fatalf(".trashed should be excluded")
	}
	if _, err := os.Stat(filepath.Join(restored, "extra")); !os.IsNotExist(err) {
		t.Fatalf("delete should remove extras")
	}
	if _, err := os.Stat(filepath.Join(dest, "keep.txt")); err != nil {
		t.Fatalf("delete touched files outside snapshot roots: %v", err)
	}

	link, err := os.Readlink(filepath.Join(restored, "link"))
	if err != nil || link != "file.txt" {
		t.Fatalf("symlink: %q err=%v", link, err)
	}
	primary, err := os.Stat(filepath.Join(restored, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	secondary, err := os.Stat(filepath.Join(restored, "dir", "hardlink"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(primary, secondary) {
		t.Fatalf("hardlink was not restored as a link")
	}
	if primary.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o", primary.Mode().Perm())
	}
	if primary.ModTime().Unix() != mtime.Unix() {
		t.Fatalf("mtime = %v", primary.ModTime())
	}

	// 第二次备份：显式 parent 应该被写进 parent / parents。
	sink2 := &frameSink{}
	request.Items[0].Parent = &info.ID
	request.Items[0].Description = nil
	request.Items[0].Tags = []string{"TestApp|1.0|owner|1800000000000"}
	if err := Backup(context.Background(), request, NewProgress(sink2.write)); err != nil {
		t.Fatalf("second backup: %v", err)
	}
	records, err = loadAllSnapshots(context.Background(), repo.Repository)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("want 2 raw snapshots, got %d", len(records))
	}
	var latest *snapshotRecord
	for _, candidate := range records {
		if latest == nil || candidate.Time.After(latest.Time) {
			latest = candidate
		}
	}
	if latest.Raw["parent"] != info.ID {
		t.Fatalf("parent = %v, want %s", latest.Raw["parent"], info.ID)
	}
	if parents, ok := latest.Raw["parents"].([]any); !ok || len(parents) != 1 || parents[0] != info.ID {
		t.Fatalf("parents = %#v", latest.Raw["parents"])
	}
}

func TestApkExcludesOnlyTopLevelFiles(t *testing.T) {
	setupRepo(t)

	src := t.TempDir()
	writeFile(t, filepath.Join(src, "base.apk"), "base")
	writeFile(t, filepath.Join(src, "split_config.apk"), "split")
	writeFile(t, filepath.Join(src, "lib", "libfoo.so"), "lib")
	writeFile(t, filepath.Join(src, "sub", "nested.apk"), "nested")
	writeFile(t, filepath.Join(src, "other.txt"), "other")

	sink := &frameSink{}
	request := &BackupRequest{Items: []BackupItem{{
		Stage:    "apk",
		Label:    "com.test:apk",
		Tags:     []string{"TestApp|1.0|owner|1700000000000"},
		Sources:  []string{src},
		AsPath:   strptr("/"),
		Excludes: []string{"**/*.apk", "!*/", "!.*/"},
	}}}
	if err := Backup(context.Background(), request, NewProgress(sink.write)); err != nil {
		t.Fatalf("backup: %v", err)
	}

	var maxTotal uint64
	for _, frame := range sink.snapshot() {
		var event struct {
			State string `json:"state"`
			Total uint64 `json:"total"`
		}
		if json.Unmarshal([]byte(frame), &event) == nil && event.State == "progress" && event.Total > maxTotal {
			maxTotal = event.Total
		}
	}
	if maxTotal != uint64(len("base")+len("split")) {
		t.Fatalf("scanner total = %d, want %d", maxTotal, len("base")+len("split"))
	}

	dest := t.TempDir()
	snapshots, err := ListSnapshots("com.test")
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("list: %v %+v", err, snapshots)
	}
	if err := Restore(context.Background(), &RestoreRequest{Items: []RestoreItem{{ID: snapshots[0].ID, Dest: dest}}}, NewProgress(nil)); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, name := range []string{"base.apk", "split_config.apk"} {
		if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
			t.Fatalf("%s should be restored: %v", name, err)
		}
	}
	for _, name := range []string{"lib", "sub", "other.txt"} {
		if _, err := os.Stat(filepath.Join(dest, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should not be restored", name)
		}
	}
}

func TestUpdateDescriptionDeleteAndCleanup(t *testing.T) {
	setupRepo(t)

	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), "a")

	sink := &frameSink{}
	request := &BackupRequest{Items: []BackupItem{{
		Stage:       "user",
		Label:       "com.test:user",
		Tags:        []string{"App|1|owner|1700000000000"},
		Sources:     []string{src},
		AsPath:      strptr("com.test"),
		Description: strptr("old"),
	}}}
	if err := Backup(context.Background(), request, NewProgress(sink.write)); err != nil {
		t.Fatal(err)
	}
	// 图标快照：包一旦没有真实备份，就应该被清掉。
	iconReq := &BackupRequest{Items: []BackupItem{{
		Stage:   "icon",
		Label:   "com.test:icon",
		Tags:    []string{"com.test"},
		Sources: []string{src},
		AsPath:  strptr("icon.png"),
	}}}
	if err := Backup(context.Background(), iconReq, NewProgress(sink.write)); err != nil {
		t.Fatal(err)
	}

	list, err := ListSnapshots("com.test")
	if err != nil {
		t.Fatal(err)
	}
	var userID, iconID string
	for _, snapshot := range list {
		if strings.HasSuffix(snapshot.Label, ":icon") {
			iconID = snapshot.ID
		} else {
			userID = snapshot.ID
		}
	}
	if userID == "" || iconID == "" {
		t.Fatalf("missing snapshots: %+v", list)
	}

	if err := UpdateDescriptions([]string{userID}, strptr("new note")); err != nil {
		t.Fatalf("update description: %v", err)
	}
	list, err = ListSnapshots("com.test")
	if err != nil {
		t.Fatal(err)
	}
	var updated *SnapshotInfo
	for i := range list {
		if list[i].Label == "com.test:user" {
			updated = &list[i]
		}
	}
	if updated == nil || updated.Description == nil || *updated.Description != "new note" {
		t.Fatalf("description not updated: %+v", updated)
	}
	if updated.ID == userID {
		t.Fatalf("snapshot id should change after edit")
	}

	if err := DeleteSnapshots([]string{updated.ID}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	overview, err := Overview()
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range overview {
		if pkg.Package == "com.test" {
			t.Fatalf("overview still has com.test after deleting its real backup: %+v", pkg)
		}
	}
	removed, err := CleanOrphanIcons()
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	list, err = ListSnapshots("")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("leftover snapshots: %+v", list)
	}

	estimate, err := PruneEstimate()
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if estimate == 0 {
		t.Fatalf("estimate should be > 0 after deleting snapshots")
	}
	if err := Prune(); err != nil {
		t.Fatalf("prune: %v", err)
	}
}

func TestRepoLayout(t *testing.T) {
	repoDir := setupRepo(t)
	layout, err := RepoLayout(repoDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !layout.Config || !layout.Keys || !layout.Data || !layout.Index || !layout.Snapshots {
		t.Fatalf("layout incomplete: %+v", layout)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	layout, err = RepoLayout(empty, nil)
	if err != nil {
		t.Fatal(err)
	}
	if layout.Config || layout.Keys || layout.Data || layout.Index || layout.Snapshots {
		t.Fatalf("empty layout should be empty: %+v", layout)
	}
}

func TestExcludeMatcher(t *testing.T) {
	tests := []struct {
		patterns []string
		path     string
		isDir    bool
		want     bool
	}{
		{[]string{"**/*.apk", "!*/", "!.*/"}, "/data/app/base.apk", false, true},
		{[]string{"**/*.apk", "!*/", "!.*/"}, "/data/app/base.txt", false, false},
		{[]string{"**/*.apk", "!*/", "!.*/"}, "/data/app/sub", true, false},
		{[]string{"**/*.apk", "!*/", "!.*/"}, "/data/app/.hidden", true, false},
		{[]string{"!.thumbnails", "!.trashed*"}, "/data/app/.thumbnails", true, false},
		{[]string{"!.thumbnails", "!.trashed*"}, "/data/app/.trashed-1", false, false},
		{[]string{"!.thumbnails", "!.trashed*"}, "/data/app/file.txt", false, true},
		{[]string{"!/data/user/*/*/cache"}, "/data/user/0/com.test/cache", true, false},
		{[]string{"!/data/user/*/*/cache"}, "/data/user/0/com.test/cache/sub", false, true},
		{[]string{"!/data/user/*/*/cache"}, "/data/user/0/com.test/other", false, true},
		{[]string{"**/*.apk"}, "/x/y/z.apk", false, true},
		{[]string{"**/*.apk"}, "/x/y/z.txt", false, false},
	}
	for _, test := range tests {
		matcher := newExcludeMatcher(test.patterns)
		if got := matcher.include(test.path, test.isDir); got != test.want {
			t.Errorf("patterns=%v path=%s dir=%v: got %v want %v",
				test.patterns, test.path, test.isDir, got, test.want)
		}
	}
}

func TestProgressFrames(t *testing.T) {
	var frames []string
	progress := NewProgress(func(frame string) { frames = append(frames, frame) })
	progress.Running("user")
	files := uint64(7)
	progress.Done("user", &files, 2)
	progress.Skipped("data", `理由 "x"\nline`)
	progress.Interrupted("user", ReasonUnreachable)
	progress.Finish()

	want := []string{
		`{"stage":"user","state":"running"}`,
		`{"stage":"user","state":"done","files":7,"errors":2}`,
		`{"stage":"data","state":"skipped","reason":"理由 \"x\"\\nline"}`,
		`{"stage":"user","state":"interrupted","reason":"unreachable"}`,
		`{"stage":"","state":"finished"}`,
	}
	if len(frames) != len(want) {
		t.Fatalf("frames = %v", frames)
	}
	for i := range want {
		if frames[i] != want[i] {
			t.Errorf("frame %d = %q, want %q", i, frames[i], want[i])
		}
	}
}

// 缓存失效只应「退休」句柄：并发查询还握着租约时，backend 不能被提前关掉。
func TestRepoLeaseOutlivesInvalidation(t *testing.T) {
	setupRepo(t)

	repo, err := openRepo()
	if err != nil {
		t.Fatal(err)
	}
	invalidateAll()

	if err := repo.List(context.Background(), restic.SnapshotFile, func(restic.ID, int64) error {
		return nil
	}); err != nil {
		t.Fatalf("leased repo was closed by invalidate: %v", err)
	}
	repo.release()

	// 退休之后重新打开应拿到一个新句柄。
	again, err := openRepo()
	if err != nil {
		t.Fatal(err)
	}
	defer again.release()
	if again == repo {
		t.Fatalf("retired repo was reused")
	}
}

func TestRepoLeaseConcurrentInvalidate(t *testing.T) {
	setupRepo(t)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				repo, err := openRepo()
				if err != nil {
					t.Errorf("open: %v", err)
					return
				}
				err = repo.List(context.Background(), restic.SnapshotFile, func(restic.ID, int64) error {
					return nil
				})
				repo.release()
				if err != nil {
					t.Errorf("list: %v", err)
					return
				}
			}
		}()
	}
	for i := 0; i < 40; i++ {
		invalidateAll()
		time.Sleep(time.Millisecond)
	}
	wg.Wait()
}

func TestBackupRepoBatchCache(t *testing.T) {
	setupRepo(t)

	batch := "batch-1"
	first, err := openBackupRepo(&batch)
	if err != nil {
		t.Fatal(err)
	}
	defer first.release()

	second, err := openBackupRepo(&batch)
	if err != nil {
		t.Fatal(err)
	}
	if second.Repository != first.Repository {
		t.Fatalf("same batch should reuse repository")
	}
	second.release()

	invalidateBackup()
	third, err := openBackupRepo(&batch)
	if err != nil {
		t.Fatal(err)
	}
	defer third.release()
	if third.Repository == first.Repository {
		t.Fatalf("invalidated batch cache should reopen repository")
	}

	// 没有批次标识时只开一次性实例，不跨调用复用。
	fourth, err := openBackupRepo(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fourth.release()
	fifth, err := openBackupRepo(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fifth.release()
	if fourth.Repository == fifth.Repository {
		t.Fatalf("one-off backup repo should not be cached")
	}
}

// 同批次备份复用同一个仓库实例时，第二轮仍要能自动找到上一轮快照做 parent，
// 并且新写入的包要能被后续读取（去重 + 增量都依赖这一点）。
func TestBackupBatchAutoParent(t *testing.T) {
	setupRepo(t)

	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), "one")
	batch := "batch-auto-parent"
	request := &BackupRequest{Batch: &batch, Items: []BackupItem{{
		Stage:   "user",
		Label:   "com.test:user",
		Tags:    []string{"App|1|owner|1700000000000"},
		Sources: []string{src},
		AsPath:  strptr("com.test"),
	}}}

	if err := Backup(context.Background(), request, NewProgress(nil)); err != nil {
		t.Fatalf("first backup: %v", err)
	}
	writeFile(t, filepath.Join(src, "a.txt"), "two")
	request.Items[0].Tags = []string{"App|1|owner|1800000000000"}
	if err := Backup(context.Background(), request, NewProgress(nil)); err != nil {
		t.Fatalf("second backup: %v", err)
	}

	repo, err := openRepo()
	if err != nil {
		t.Fatal(err)
	}
	defer repo.release()
	records, err := loadAllSnapshots(context.Background(), repo.Repository)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("want 2 snapshots, got %d", len(records))
	}
	var latest *snapshotRecord
	for _, rec := range records {
		if latest == nil || rec.Time.After(latest.Time) {
			latest = rec
		}
	}
	if latest.Raw["parent"] == nil {
		t.Fatalf("second backup did not select a parent: %+v", latest.Raw)
	}
}
