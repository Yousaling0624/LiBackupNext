package engine

import (
	"context"
	"os"
	"path/filepath"

	"github.com/restic/restic/internal/backend"
	"github.com/restic/restic/internal/repository"
	"github.com/restic/restic/internal/restic"
)

// RepoLayout 查五类文件在不在，不需要密码、不打开仓库。
func RepoLayout(path string, options map[string]string) (*Layout, error) {
	if root, ok := localRoot(path); ok {
		return &Layout{
			Config:    isFile(filepath.Join(root, "config")),
			Keys:      isDir(filepath.Join(root, "keys")),
			Data:      isDir(filepath.Join(root, "data")),
			Index:     isDir(filepath.Join(root, "index")),
			Snapshots: isDir(filepath.Join(root, "snapshots")),
		}, nil
	}

	// 远程只有 config 可信：restic 协议对不存在的目录返回 200 + 空列表，
	// keys/data/index/snapshots 的「列成功 = 存在」全是假阳性。
	be, _, err := makeBackend(context.Background(), path, options, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = be.Close() }()
	_, err = be.Stat(context.Background(), backend.Handle{Type: restic.ConfigFile})
	complete := err == nil
	if err != nil && !be.IsNotExist(err) {
		return nil, wrapFailure("", err)
	}
	return &Layout{
		Config:    complete,
		Keys:      complete,
		Data:      complete,
		Index:     complete,
		Snapshots: complete,
	}, nil
}

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// InitRepo 建库或校验已有仓库。候选凭据只用于这一次请求，不写全局配置。
func InitRepo(request *InitRequest) error {
	ctx := context.Background()
	be, _, err := makeBackend(ctx, request.Path, request.Options, request.ForceInit)
	if err != nil {
		return err
	}
	repo, err := repository.New(be, repository.Options{})
	if err != nil {
		return wrapFailure("", err)
	}
	defer func() { _ = repo.Close() }()
	if request.ForceInit {
		if err := repo.Init(ctx, 2, request.Password, nil); err != nil {
			return wrapFailure("", err)
		}
		return nil
	}
	if err := repo.SearchKey(ctx, request.Password, 0, ""); err != nil {
		return wrapFailure("", err)
	}
	return nil
}
