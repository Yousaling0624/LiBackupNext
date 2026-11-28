package engine

import (
	"context"
	"math"

	"github.com/restic/restic/internal/repository"
	"github.com/restic/restic/internal/restic"
	"github.com/restic/restic/internal/ui/progress"
)

func pruneOptions() repository.PruneOptions {
	return repository.PruneOptions{
		// 立刻回收：不标记等待，也不保留 23 小时
		MaxUnusedBytes: func(uint64) uint64 { return 0 },
		MaxRepackBytes: math.MaxUint64,
	}
}

func getUsedBlobs(ctx context.Context, repo restic.Repository, used restic.FindBlobSet) error {
	var trees restic.IDs
	err := restic.ForAllSnapshots(ctx, repo, repo, nil, func(_ restic.ID, snapshot *restic.Snapshot, err error) error {
		if err != nil {
			return err
		}
		if snapshot.Tree != nil {
			trees = append(trees, *snapshot.Tree)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return restic.FindUsedBlobs(ctx, repo, trees, used, nil)
}

func planPrune(ctx context.Context, repo *repository.Repository) (*repository.PrunePlan, error) {
	plan, err := repository.PlanPrune(ctx, pruneOptions(), repo, getUsedBlobs, &progress.NoopPrinter{})
	if err != nil {
		return nil, wrapFailure("", err)
	}
	return plan, nil
}

// Prune 回收无引用数据。和 Rust 一样，整理前后都要让索引缓存失效。
func Prune() error {
	leave := enterWriteGate()
	defer leave()

	invalidateIndexed()
	invalidateBackup()
	defer invalidateIndexed()
	defer invalidateBackup()

	repo, err := openIndexedRepo()
	if err != nil {
		return err
	}
	defer repo.release()
	plan, err := planPrune(context.Background(), repo.Repository)
	if err != nil {
		return err
	}
	if err := plan.Execute(context.Background(), &progress.NoopPrinter{}); err != nil {
		return wrapFailure("", err)
	}
	return nil
}

// PruneEstimate 只算不删。Rustic 的估算里把「整包删除」按完整 pack 文件大小再算一遍，
// restic 的公有计划只暴露其中未使用 blob 的字节数；这里用 2*Remove 近似那一份，
// 误差只在 pack 头部 / 压缩开销这一小块。
func PruneEstimate() (uint64, error) {
	repo, err := openIndexedRepo()
	if err != nil {
		return 0, err
	}
	defer repo.release()
	plan, err := planPrune(context.Background(), repo.Repository)
	if err != nil {
		return 0, err
	}
	stats := plan.Stats()
	total := uint64(0)
	for _, part := range []uint64{stats.Size.Remove, stats.Size.Remove, stats.Size.Repackrm, stats.Size.Unref} {
		if math.MaxUint64-total < part {
			return math.MaxUint64, nil
		}
		total += part
	}
	return total, nil
}
