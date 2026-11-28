package engine

// 与 Kotlin 侧 RusticProtocol.kt / RcloneProtocol.kt 对应的请求和响应结构。
// 字段名是跨进程 wire 协议，不能改。

// BackupItem 是 RusticServer 备份请求里的一个快照。
type BackupItem struct {
	Stage       string   `json:"stage"`
	Label       string   `json:"label"`
	Tags        []string `json:"tags"`
	Sources     []string `json:"sources"`
	AsPath      *string  `json:"as_path"`
	SkipReason  *string  `json:"skip_reason"`
	Description *string  `json:"description"`
	Parent      *string  `json:"parent"`
	Excludes    []string `json:"excludes"`
	IgnoreInode bool     `json:"ignore_inode"`
	// Kotlin 还会带 system 等字段，这里忽略。
}

type BackupRequest struct {
	Items    []BackupItem `json:"items"`
	Excludes []string     `json:"excludes"`
	Batch    *string      `json:"batch"`
}

type InitRequest struct {
	Path      string            `json:"path"`
	Password  string            `json:"password"`
	Options   map[string]string `json:"options"`
	ForceInit bool              `json:"force_init"`
}

type RepoLayoutRequest struct {
	Path    string            `json:"path"`
	Options map[string]string `json:"options"`
}

type ListRequest struct {
	PackageName string `json:"package_name"`
}

type UpdateDescriptionRequest struct {
	IDs         []string `json:"ids"`
	Description *string  `json:"description"`
}

type DeleteRequest struct {
	IDs []string `json:"ids"`
}

type RestoreItem struct {
	ID     string `json:"id"`
	Dest   string `json:"dest"`
	Delete bool   `json:"delete"`
	Stage  string `json:"stage"`
}

type RestoreRequest struct {
	Items []RestoreItem `json:"items"`
}

// Layout 是仓库五类文件的存在情况；完整/缺失由主进程判定。
type Layout struct {
	Config    bool
	Keys      bool
	Data      bool
	Index     bool
	Snapshots bool
}

type SnapshotInfo struct {
	ID          string  `json:"id"`
	Label       string  `json:"label"`
	Tags        string  `json:"tags"`
	Time        int64   `json:"time"`
	Size        uint64  `json:"size"`
	Description *string `json:"description,omitempty"`
}

type ListResponse struct {
	Ok        bool           `json:"ok"`
	Snapshots []SnapshotInfo `json:"snapshots"`
	Error     string         `json:"error,omitempty"`
	Reason    string         `json:"reason,omitempty"`
}

func NewListResponse(snapshots []SnapshotInfo) ListResponse {
	if snapshots == nil {
		snapshots = []SnapshotInfo{}
	}
	return ListResponse{Ok: true, Snapshots: snapshots}
}

func ListFailure(f *Failure) ListResponse {
	return ListResponse{Ok: false, Snapshots: []SnapshotInfo{}, Error: f.Message, Reason: f.ReasonWire()}
}

type OverviewSnapshot struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Size  uint64 `json:"size"`
}

type PackageOverview struct {
	Package     string             `json:"package"`
	Count       uint32             `json:"count"`
	Time        int64              `json:"time"`
	Tags        string             `json:"tags"`
	Description *string            `json:"description,omitempty"`
	Snapshots   []OverviewSnapshot `json:"snapshots"`
	HasApk      bool               `json:"has_apk"`
	ApkTags     *string            `json:"apk_tags,omitempty"`
	ApkTime     int64              `json:"apk_time"`
	HasIcon     bool               `json:"has_icon"`
}

type OverviewResponse struct {
	Ok       bool              `json:"ok"`
	Packages []PackageOverview `json:"packages"`
	Error    string            `json:"error,omitempty"`
	Reason   string            `json:"reason,omitempty"`
}

func NewOverviewResponse(packages []PackageOverview) OverviewResponse {
	if packages == nil {
		packages = []PackageOverview{}
	}
	return OverviewResponse{Ok: true, Packages: packages}
}

func OverviewFailure(f *Failure) OverviewResponse {
	return OverviewResponse{Ok: false, Packages: []PackageOverview{}, Error: f.Message, Reason: f.ReasonWire()}
}

type LayoutResponse struct {
	Ok        bool   `json:"ok"`
	Config    bool   `json:"config"`
	Keys      bool   `json:"keys"`
	Data      bool   `json:"data"`
	Index     bool   `json:"index"`
	Snapshots bool   `json:"snapshots"`
	Error     string `json:"error,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func NewLayoutResponse(l Layout) LayoutResponse {
	return LayoutResponse{Ok: true, Config: l.Config, Keys: l.Keys, Data: l.Data, Index: l.Index, Snapshots: l.Snapshots}
}

func LayoutFailure(f *Failure) LayoutResponse {
	return LayoutResponse{Ok: false, Error: f.Message, Reason: f.ReasonWire()}
}

type CleanupResponse struct {
	Ok      bool   `json:"ok"`
	Removed uint32 `json:"removed"`
	Error   string `json:"error,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

func NewCleanupResponse(removed uint32) CleanupResponse {
	return CleanupResponse{Ok: true, Removed: removed}
}

func CleanupFailure(f *Failure) CleanupResponse {
	return CleanupResponse{Ok: false, Removed: 0, Error: f.Message, Reason: f.ReasonWire()}
}

type EstimateResponse struct {
	Ok     bool   `json:"ok"`
	Bytes  uint64 `json:"bytes"`
	Error  string `json:"error,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func NewEstimateResponse(bytes uint64) EstimateResponse {
	return EstimateResponse{Ok: true, Bytes: bytes}
}

func EstimateFailure(f *Failure) EstimateResponse {
	return EstimateResponse{Ok: false, Bytes: 0, Error: f.Message, Reason: f.ReasonWire()}
}

type BridgeResponse struct {
	Ok     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func OKResponse() BridgeResponse { return BridgeResponse{Ok: true} }

func FailureResponse(f *Failure) BridgeResponse {
	return BridgeResponse{Ok: false, Error: f.Message, Reason: f.ReasonWire()}
}

// Rclone 侧请求。
type ListDirsRequest struct {
	Fs     string  `json:"fs"`
	Path   string  `json:"path"`
	Create *string `json:"create"`
	// lib 是旧 Rust 桥的字段；合并成单个库后不再使用，留着是为了兼容旧请求。
	Lib string `json:"lib"`
}

type EnsureServeRequest struct {
	Fs  string `json:"fs"`
	Key string `json:"key"`
	Lib string `json:"lib"`
}
