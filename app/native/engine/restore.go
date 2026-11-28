package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/restic/restic/internal/repository"
	"github.com/restic/restic/internal/restic"
)

const (
	restoreThreads     = 4
	restoreChunk       = 1024 * 1024
	restoreNice        = 10
	restoreFileThreads = 4
	restoreParallelMin = 4 * 1024 * 1024
)

// Restore 解包一批快照。和 Rust 实现一样，这里只负责字节进度，
// 不写 running / done / finished 事件（上层恢复流程有自己的步骤状态）。
func Restore(ctx context.Context, request *RestoreRequest, progress *Progress) error {
	// 恢复只读仓库，但读的包可能正被整理仓库删掉，所以和写操作互斥。
	leave := enterWriteGate()
	defer leave()

	if len(request.Items) == 0 {
		return nil
	}
	repo, err := openIndexedRepo()
	if err != nil {
		return err
	}
	defer repo.release()

	for i := range request.Items {
		item := &request.Items[i]
		snapshot, err := loadSnapshotByID(ctx, repo.Repository, item.ID)
		if err != nil {
			return wrapFailure("", err)
		}
		if snapshot == nil {
			return appFailuref("备份里没有这个快照：%s", item.ID)
		}

		progress.SetStage(item.Stage)
		bytes := progress.NewBytes()
		if err := restoreSnapshot(ctx, repo.Repository, snapshot, item.Dest, item.Delete, bytes); err != nil {
			return wrapFailure("恢复 "+item.ID+" 失败：", err)
		}
	}
	return nil
}

type restoreDir struct {
	target string
	mode   os.FileMode
	mtime  time.Time
}

type restoreSymlink struct {
	target string
	link   string
}

type restoreHardlink struct {
	target  string
	primary string
}

type restoreFile struct {
	target string
	node   *restic.Node
}

type blobRange struct {
	id    restic.ID
	start uint64
	end   uint64
}

func restoreSnapshot(ctx context.Context, repo *repository.Repository, snapshot *restic.Snapshot, dest string, deleteExtras bool, progress *BytesProgress) error {
	if snapshot.Tree == nil {
		return appFailure("快照没有根目录")
	}
	dest = filepath.Clean(dest)

	var (
		written   = make(map[string]struct{})
		dirs      []restoreDir
		files     []restoreFile
		symlinks  []restoreSymlink
		primaries = make(map[[2]uint64]string)
		hardlinks []restoreHardlink
	)

	if err := collectTree(ctx, repo, *snapshot.Tree, "", dest, written, &dirs, &files, &symlinks, primaries, &hardlinks); err != nil {
		return err
	}

	if len(files) > 0 {
		var total uint64
		for _, file := range files {
			total += file.node.Size
		}
		progress.SetTotal(total)

		if err := restoreFiles(ctx, repo, files, progress); err != nil {
			return err
		}
		progress.Finish()
	}

	for _, link := range hardlinks {
		removeExisting(link.target)
		if err := os.Link(link.primary, link.target); err != nil {
			return fmt.Errorf("%s: %w", link.target, err)
		}
	}

	for _, link := range symlinks {
		removeExisting(link.target)
		if err := os.Symlink(link.link, link.target); err != nil {
			return fmt.Errorf("%s: %w", link.target, err)
		}
	}

	sort.SliceStable(dirs, func(i, j int) bool {
		return pathDepth(dirs[i].target) > pathDepth(dirs[j].target)
	})
	for _, dir := range dirs {
		if dir.mode != 0 {
			setMode(dir.target, dir.mode)
		}
	}
	for _, dir := range dirs {
		if !dir.mtime.IsZero() {
			_ = os.Chtimes(dir.target, dir.mtime, dir.mtime)
		}
	}

	if deleteExtras {
		removeRestoreExtras(dest, written)
	}
	return nil
}

func collectTree(
	ctx context.Context,
	repo *repository.Repository,
	treeID restic.ID,
	rel string,
	dest string,
	written map[string]struct{},
	dirs *[]restoreDir,
	files *[]restoreFile,
	symlinks *[]restoreSymlink,
	primaries map[[2]uint64]string,
	hardlinks *[]restoreHardlink,
) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	tree, err := restic.LoadTree(ctx, repo, treeID)
	if err != nil {
		return err
	}
	nodes := append([]*restic.Node(nil), tree.Nodes...)
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

	for _, node := range nodes {
		target, newRel, err := joinRestorePath(dest, rel, node.Name)
		if err != nil {
			return err
		}
		written[newRel] = struct{}{}

		isDir := node.Type == restic.NodeTypeDir || (node.Subtree != nil && node.Type == "")
		if isDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("%s: %w", target, err)
			}
			*dirs = append(*dirs, restoreDir{target: target, mode: node.Mode, mtime: node.ModTime})
			if node.Subtree != nil {
				if err := collectTree(ctx, repo, *node.Subtree, newRel, dest, written, dirs, files, symlinks, primaries, hardlinks); err != nil {
					return err
				}
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("%s: %w", filepath.Dir(target), err)
		}

		switch node.Type {
		case restic.NodeTypeSymlink:
			*symlinks = append(*symlinks, restoreSymlink{target: target, link: node.LinkTarget})
			continue
		case restic.NodeTypeFile:
		default:
			// fifo / socket / 设备节点在应用数据目录里还原不了，跳过。
			continue
		}

		if node.Inode != 0 {
			key := [2]uint64{node.DeviceID, node.Inode}
			if primary, ok := primaries[key]; ok {
				*hardlinks = append(*hardlinks, restoreHardlink{target: target, primary: primary})
				continue
			}
			primaries[key] = target
		}
		*files = append(*files, restoreFile{target: target, node: node})
	}
	return nil
}

func joinRestorePath(dest, rel, name string) (string, string, error) {
	if name == "" {
		return "", "", appFailure("快照里的路径不合法（空名称）")
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return "", "", appFailuref("快照里的路径不合法（越出恢复目标）：%s/%s", rel, name)
		}
	}
	newRel := name
	if rel != "" {
		newRel = rel + "/" + name
	}
	target := filepath.Join(dest, filepath.FromSlash(newRel))
	cleanDest := filepath.Clean(dest)
	if target != cleanDest && !strings.HasPrefix(target, cleanDest+string(filepath.Separator)) {
		return "", "", appFailuref("快照里的路径不合法（越出恢复目标）：%s", newRel)
	}
	return target, newRel, nil
}

func pathDepth(p string) int {
	return strings.Count(filepath.Clean(p), string(filepath.Separator))
}

func restoreFiles(ctx context.Context, repo *repository.Repository, files []restoreFile, progress *BytesProgress) error {
	workers := restoreThreads
	if cores := runtime.NumCPU() - 1; cores < workers {
		workers = cores
	}
	if workers < 1 {
		workers = 1
	}
	if workers > len(files) {
		workers = len(files)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan restoreFile)
	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	reportErr := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		errMu.Unlock()
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			_ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, restoreNice)
			for file := range jobs {
				if ctx.Err() != nil {
					return
				}
				if err := restoreFileData(ctx, repo, file.node, file.target); err != nil {
					reportErr(err)
					return
				}
				progress.Add(file.node.Size)
			}
		}()
	}

	for _, file := range files {
		select {
		case jobs <- file:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()

	errMu.Lock()
	defer errMu.Unlock()
	return firstErr
}

func restoreFileData(ctx context.Context, repo *repository.Repository, node *restic.Node, target string) error {
	blobs, err := nodeBlobRanges(repo, node)
	if err != nil {
		return fmt.Errorf("%s: %w", target, err)
	}
	removeExisting(target)
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o666)
	if err != nil {
		return fmt.Errorf("%s: %w", target, err)
	}
	closeFile := true
	defer func() {
		if closeFile {
			_ = file.Close()
		}
	}()

	size := node.Size
	if size < restoreParallelMin {
		if err := writeBlobRange(ctx, repo, file, blobs, 0, size); err != nil {
			return fmt.Errorf("%s: %w", target, err)
		}
	} else {
		segments := uint64(restoreFileThreads)
		if maxSeg := (size + restoreParallelMin - 1) / restoreParallelMin; maxSeg < segments {
			segments = maxSeg
		}
		if segments == 0 {
			segments = 1
		}
		segmentSize := (size + segments - 1) / segments
		var (
			wg       sync.WaitGroup
			errMu    sync.Mutex
			firstErr error
		)
		for index := uint64(0); index < segments; index++ {
			start := index * segmentSize
			end := start + segmentSize
			if end > size {
				end = size
			}
			if start >= end {
				continue
			}
			wg.Add(1)
			go func(start, end uint64) {
				defer wg.Done()
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()
				_ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, restoreNice)
				if err := writeBlobRange(ctx, repo, file, blobs, start, end); err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
				}
			}(start, end)
		}
		wg.Wait()
		if firstErr != nil {
			return fmt.Errorf("%s: %w", target, firstErr)
		}
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("%s: %w", target, err)
	}
	closeFile = false

	if node.Mode != 0 {
		setMode(target, node.Mode)
	}
	if !node.ModTime.IsZero() {
		_ = os.Chtimes(target, node.ModTime, node.ModTime)
	}
	return nil
}

func nodeBlobRanges(repo *repository.Repository, node *restic.Node) ([]blobRange, error) {
	if len(node.Content) == 0 {
		if node.Size == 0 {
			return nil, nil
		}
		return nil, appFailure("文件内容缺失")
	}
	blobs := make([]blobRange, 0, len(node.Content))
	var offset uint64
	for _, id := range node.Content {
		size, ok := repo.LookupBlobSize(restic.DataBlob, id)
		if !ok {
			return nil, fmt.Errorf("id %v not found in repository", id)
		}
		blobs = append(blobs, blobRange{id: id, start: offset, end: offset + uint64(size)})
		offset += uint64(size)
	}
	if offset < node.Size {
		return nil, appFailure("文件内容不完整")
	}
	return blobs, nil
}

func writeBlobRange(ctx context.Context, repo *repository.Repository, file *os.File, blobs []blobRange, start, end uint64) error {
	index := 0
	for index < len(blobs) && blobs[index].end <= start {
		index++
	}
	if index == len(blobs) {
		return nil
	}
	for offset := start; offset < end; {
		for index < len(blobs) && blobs[index].end <= offset {
			index++
		}
		if index >= len(blobs) {
			return appFailure("文件内容不完整")
		}
		blob := blobs[index]
		data, err := repo.LoadBlob(ctx, restic.DataBlob, blob.id, nil)
		if err != nil {
			return err
		}
		blobStart := blob.start
		if offset < blobStart {
			return appFailure("文件内容不完整")
		}
		blobOffset := offset - blobStart
		if blobOffset >= uint64(len(data)) {
			index++
			continue
		}
		chunk := end - offset
		available := uint64(len(data)) - blobOffset
		if chunk > available {
			chunk = available
		}
		slice := data[blobOffset : blobOffset+chunk]
		if _, err := file.WriteAt(slice, int64(offset)); err != nil {
			return err
		}
		offset += chunk
		if blobOffset+chunk >= uint64(len(data)) {
			index++
		}
	}
	return nil
}

func removeExisting(target string) {
	fi, err := os.Lstat(target)
	if err != nil {
		return
	}
	if fi.IsDir() {
		_ = os.RemoveAll(target)
	} else {
		_ = os.Remove(target)
	}
}

func setMode(target string, mode os.FileMode) {
	perm := mode.Perm() | (mode & (os.ModeSetuid | os.ModeSetgid | os.ModeSticky))
	_ = os.Chmod(target, perm)
}

func removeRestoreExtras(dest string, written map[string]struct{}) {
	roots := make(map[string]struct{})
	for rel := range written {
		first := strings.SplitN(rel, "/", 2)[0]
		if first == "" || first == "." || first == ".." {
			continue
		}
		roots[first] = struct{}{}
	}

	for first := range roots {
		root := filepath.Join(dest, filepath.FromSlash(first))
		var extras []string
		_ = filepath.WalkDir(root, func(current string, entry os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, err := filepath.Rel(dest, current)
			if err != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if _, ok := written[rel]; !ok {
				extras = append(extras, current)
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			return nil
		})

		sort.Slice(extras, func(i, j int) bool {
			return pathDepth(extras[i]) > pathDepth(extras[j])
		})
		for _, extra := range extras {
			fi, err := os.Lstat(extra)
			if err == nil && fi.IsDir() {
				_ = os.RemoveAll(extra)
			} else {
				_ = os.Remove(extra)
			}
		}
	}
}
