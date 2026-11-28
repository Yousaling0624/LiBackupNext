package engine

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/restic/restic/internal/archiver"
	"github.com/restic/restic/internal/fs"
	"github.com/restic/restic/internal/repository"
	"github.com/restic/restic/internal/restic"
)

// errorCounter 统计「读不出来的源文件」个数。Rustic 的 error_count 是按错误
// 发生次数累加的；这里按路径去重，避免同一个文件 open + read 各算一次。
type errorCounter struct {
	mu    sync.Mutex
	paths map[string]struct{}
}

func newErrorCounter() *errorCounter {
	return &errorCounter{paths: make(map[string]struct{})}
}

func (c *errorCounter) add(path string) {
	if path == "" || path == "/" {
		return
	}
	c.mu.Lock()
	c.paths[path] = struct{}{}
	c.mu.Unlock()
}

func (c *errorCounter) count() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return uint64(len(c.paths))
}

// Backup 执行一批备份项。进度和 rust 实现一样：每个 item 先 running，
// 源路径全不存在则 skipped，成功写快照后 done。
func Backup(ctx context.Context, request *BackupRequest, progress *Progress) (resultErr error) {
	// 整批备份占住写闸门；期间 prune / 删除都要排队，
	// 否则刚写完还没进索引的包会被当成无引用数据回收掉。
	leave := enterWriteGate()
	defer leave()

	invalidateIndexed()
	repo, err := openBackupRepo(request.Batch)
	if err != nil {
		invalidateIndexed()
		return err
	}
	defer repo.release()
	defer invalidateIndexed()
	// 失败后缓存的索引可能和磁盘对不上，同批次下一笔必须重开。
	defer func() {
		if resultErr != nil {
			invalidateBackup()
		}
	}()

	for i := range request.Items {
		item := &request.Items[i]
		progress.Running(item.Stage)

		var sources []string
		for _, source := range item.Sources {
			if _, err := os.Stat(source); err == nil {
				sources = append(sources, source)
			}
		}
		if len(sources) == 0 {
			reason := ""
			if item.SkipReason != nil {
				reason = *item.SkipReason
			} else {
				reason = "源路径不存在：" + strings.Join(item.Sources, ", ")
			}
			progress.Skipped(item.Stage, reason)
			continue
		}

		if err := backupItem(ctx, repo.Repository, request, item, sources, progress); err != nil {
			return err
		}
	}
	return nil
}

func backupItem(ctx context.Context, repo *repository.Repository, request *BackupRequest, item *BackupItem, sources []string, progress *Progress) error {
	if item.AsPath != nil && len(sources) > 1 {
		return appFailure("不支持多个备份源使用同一个 as_path")
	}

	hostname, _ := os.Hostname()
	start := time.Now()

	tags := normalizeTags(item.Tags)
	paths := append([]string(nil), sources...)
	targets := append([]string(nil), sources...)
	virtualRoot := sources[0]
	if item.AsPath != nil {
		virtualRoot = *item.AsPath
		paths = []string{virtualRoot}
		targets = []string{virtualRoot}
	}
	paths = sortedUnique(paths)

	var parent *restic.Snapshot
	if item.Parent != nil && strings.TrimSpace(*item.Parent) != "" {
		// 显式 parent 找不到时就用无 parent 的全量备份；不要偷偷换成「最新」，
		// 那会和客户端挑 parent 的语义不一致。
		loaded, err := loadSnapshotByID(ctx, repo, strings.TrimSpace(*item.Parent))
		if err != nil {
			return wrapFailure("", err)
		}
		parent = loaded
	} else {
		var err error
		parent, err = findParent(ctx, repo, hostname, item.Label, paths)
		if err != nil {
			return wrapFailure("", err)
		}
	}

	excludes := request.Excludes
	if len(item.Excludes) > 0 {
		excludes = item.Excludes
	}
	matcher := newExcludeMatcher(excludes)

	vfs := newVirtualFS(sources[0], virtualRoot)
	arch := archiver.New(repo, vfs, archiver.Options{})
	arch.SelectByName = func(string) bool { return true }
	arch.Select = func(name string, fi *fs.ExtendedFileInfo, _ fs.FS) bool {
		return matcher.include(name, fi.Mode.IsDir())
	}
	if item.IgnoreInode {
		arch.ChangeIgnoreFlags = archiver.ChangeIgnoreInode
	}

	counted := newErrorCounter()
	arch.Error = func(filename string, err error) error {
		if errors.Is(err, context.Canceled) {
			return err
		}
		if reason := classifyError(err); reason.isTransport() {
			return err
		}
		counted.add(filename)
		return nil
	}

	bytes := progress.NewBytes()
	var scanWg sync.WaitGroup
	scanWg.Add(1)
	go func() {
		defer scanWg.Done()
		scanner := archiver.NewScanner(vfs)
		scanner.SelectByName = func(string) bool { return true }
		// Scanner 传进来的是虚拟路径；排除规则和错误计数都按真实路径判断，
		// 和 archiver（它传 Abs 后的真实路径）保持一致。
		scanner.Select = func(name string, fi *fs.ExtendedFileInfo, _ fs.FS) bool {
			real, err := vfs.Abs(name)
			if err != nil {
				return matcher.include(name, fi.Mode.IsDir())
			}
			return matcher.include(real, fi.Mode.IsDir())
		}
		scanner.Error = func(filename string, err error) error {
			if reason := classifyError(err); reason.isTransport() {
				return err
			}
			real, mapErr := vfs.Abs(filename)
			if mapErr != nil {
				real = filename
			}
			counted.add(real)
			return nil
		}
		scanner.Result = func(name string, stats archiver.ScanStats) {
			if name == "" {
				bytes.SetTotal(stats.Bytes)
			}
		}
		_ = scanner.Scan(ctx, targets)
	}()

	arch.CompleteBlob = func(n uint64) { bytes.Add(n) }

	snapOpts := archiver.SnapshotOptions{
		Tags:           tags,
		Hostname:       hostname,
		BackupStart:    start,
		Time:           start,
		ParentSnapshot: parent,
		ProgramVersion: "rustic 0.14.0",
	}
	autoSnapshot, autoID, summary, err := arch.Snapshot(ctx, targets, snapOpts)
	scanWg.Wait()
	bytes.Finish()
	if err != nil {
		return wrapFailure("备份 "+item.Stage+" 失败：", err)
	}
	if autoSnapshot == nil || autoSnapshot.Tree == nil || summary == nil {
		return appFailuref("备份 %s 失败：快照为空", item.Stage)
	}

	errorCount := counted.count()
	files := summary.Files.New + summary.Files.Changed + summary.Files.Unchanged
	dirs := summary.Dirs.New + summary.Dirs.Changed + summary.Dirs.Unchanged
	end := summary.BackupEnd
	if end.IsZero() {
		end = time.Now()
	}

	parentID := ""
	if parent != nil && parent.ID() != nil {
		parentID = parent.ID().String()
	}
	var parentPtr *string
	var parents []string
	if parentID != "" {
		parentPtr = &parentID
		parents = []string{parentID}
	}

	itemStats := summary.ItemStats
	snap := &snapshotFileJSON{
		Time:           formatRusticTime(start),
		ProgramVersion: "rustic 0.14.0",
		Parent:         parentPtr,
		Parents:        parents,
		Tree:           autoSnapshot.Tree.String(),
		Label:          item.Label,
		Paths:          paths,
		Hostname:       hostname,
		Username:       "",
		UID:            0,
		GID:            0,
		Tags:           tags,
		Description:    item.Description,
		Summary: &snapshotSummary{
			FilesNew:              uint64(summary.Files.New),
			FilesChanged:          uint64(summary.Files.Changed),
			FilesUnmodified:       uint64(summary.Files.Unchanged),
			TotalFilesProcessed:   uint64(files),
			TotalBytesProcessed:   summary.ProcessedBytes,
			DirsNew:               uint64(summary.Dirs.New),
			DirsChanged:           uint64(summary.Dirs.Changed),
			DirsUnmodified:        uint64(summary.Dirs.Unchanged),
			TotalDirsProcessed:    uint64(dirs),
			TotalDirsizeProcessed: 0,
			DataBlobs:             uint64(itemStats.DataBlobs),
			TreeBlobs:             uint64(itemStats.TreeBlobs),
			DataAdded:             itemStats.DataSize + itemStats.TreeSize,
			DataAddedPacked:       itemStats.DataSizeInRepo + itemStats.TreeSizeInRepo,
			DataAddedFiles:        itemStats.DataSize,
			DataAddedFilesPacked:  itemStats.DataSizeInRepo,
			DataAddedTrees:        itemStats.TreeSize,
			DataAddedTreesPacked:  itemStats.TreeSizeInRepo,
			Command:               "",
			BackupStart:           formatRusticTime(summary.BackupStart),
			BackupEnd:             formatRusticTime(end),
			BackupDuration:        end.Sub(summary.BackupStart).Seconds(),
			TotalDuration:         end.Sub(start).Seconds(),
			ErrorCount:            errorCount,
		},
	}

	if _, err := saveCustomSnapshot(ctx, repo, autoID, snap); err != nil {
		return wrapFailure("备份 "+item.Stage+" 失败：", err)
	}

	progress.Done(item.Stage, ptrU64(uint64(files)), errorCount)
	return nil
}

func ptrU64(v uint64) *uint64 { return &v }

func normalizeTags(values []string) []string {
	if len(values) == 0 {
		// rustic 的 StringList 反序列化只接受数组，空 tags 写成 [] 而不是 null。
		return []string{}
	}
	joined := strings.Join(values, ",")
	return sortedUnique(strings.Split(joined, ","))
}

func loadSnapshotByID(ctx context.Context, repo *repository.Repository, requested string) (*restic.Snapshot, error) {
	var found restic.ID
	err := repo.List(ctx, restic.SnapshotFile, func(id restic.ID, _ int64) error {
		if strings.HasPrefix(id.String(), requested) {
			found = id
			return nil
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if found.IsNull() {
		return nil, nil
	}
	sn, err := restic.LoadSnapshot(ctx, repo, found)
	if err != nil {
		return nil, err
	}
	return sn, nil
}
