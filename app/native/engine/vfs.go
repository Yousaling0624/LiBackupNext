package engine

import (
	"path"
	"strings"

	"github.com/restic/restic/internal/fs"
)

// virtualFS 把快照里的虚拟路径（as_path 或源路径）映射到真实文件系统路径。
// restic archiver 看到的路径名是虚拟的（决定快照里的目录结构），打开文件时
// 落到真实路径；排除规则则始终按真实路径判断，和 rustic 的 LocalSource 一致。
type virtualFS struct {
	local       fs.Local
	realRoot    string
	virtualRoot string
}

var _ fs.FS = (*virtualFS)(nil)

func newVirtualFS(realRoot, virtualRoot string) *virtualFS {
	return &virtualFS{
		realRoot:    path.Clean(realRoot),
		virtualRoot: path.Clean(virtualRoot),
	}
}

func (v *virtualFS) realPath(name string) string {
	if name == "" {
		return v.realRoot
	}
	clean := path.Clean(name)

	// scanner 会把虚拟路径经 Abs 转成真实路径后再传进来；已经映射过的直接放行。
	if clean == v.realRoot || strings.HasPrefix(clean, v.realRoot+"/") {
		return clean
	}

	vr := v.virtualRoot
	if vr == "/" || vr == "." {
		if clean == "/" || clean == "." || clean == vr {
			return v.realRoot
		}
		return path.Join(v.realRoot, strings.TrimPrefix(clean, "/"))
	}

	if clean == vr {
		return v.realRoot
	}
	if strings.HasPrefix(clean, vr+"/") {
		return path.Join(v.realRoot, strings.TrimPrefix(clean, vr+"/"))
	}

	return clean
}

func (v *virtualFS) OpenFile(name string, flag int, metadataOnly bool) (fs.File, error) {
	return v.local.OpenFile(v.realPath(name), flag, metadataOnly)
}

func (v *virtualFS) Lstat(name string) (*fs.ExtendedFileInfo, error) {
	return v.local.Lstat(v.realPath(name))
}

func (v *virtualFS) Join(elem ...string) string { return path.Join(elem...) }

func (v *virtualFS) Separator() string { return "/" }

func (v *virtualFS) Abs(name string) (string, error) {
	mapped := v.realPath(name)
	if !path.IsAbs(mapped) {
		mapped = path.Join("/", mapped)
	}
	return mapped, nil
}

func (v *virtualFS) Clean(p string) string      { return path.Clean(p) }
func (v *virtualFS) VolumeName(_ string) string { return "" }
func (v *virtualFS) IsAbs(p string) bool        { return path.IsAbs(p) }
func (v *virtualFS) Dir(p string) string        { return path.Dir(p) }
func (v *virtualFS) Base(p string) string       { return path.Base(p) }
