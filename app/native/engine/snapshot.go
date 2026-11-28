package engine

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/restic/restic/internal/repository"
	"github.com/restic/restic/internal/restic"
)

// snapshotRecord 是仓库里一条快照的原始 JSON + 常用字段。Rust 侧用的是 rustic 的
// SnapshotFile（label / description / parents / summary 全都保留），restic 自带的
// LoadSnapshot 会丢掉这些字段，所以这里直接读原始 JSON。
type snapshotRecord struct {
	ID          restic.ID
	Label       string
	Tags        []string
	Time        time.Time
	Description *string
	Size        uint64
	Hostname    string
	Paths       []string
	Raw         map[string]any
}

func parseSnapshotRecord(id restic.ID, data []byte) (*snapshotRecord, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}

	timeStr, _ := raw["time"].(string)
	t, err := parseRusticTime(timeStr)
	if err != nil {
		return nil, err
	}

	rec := &snapshotRecord{
		ID:       id,
		Label:    stringValue(raw["label"]),
		Tags:     stringListValue(raw["tags"]),
		Time:     t,
		Hostname: stringValue(raw["hostname"]),
		Paths:    sortedUnique(stringListValue(raw["paths"])),
		Raw:      raw,
	}
	rec.Description = optionalString(raw["description"])
	if summary, ok := raw["summary"].(map[string]any); ok {
		rec.Size = uintValue(summary["total_bytes_processed"])
	}
	return rec, nil
}

func parseRusticTime(value string) (time.Time, error) {
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.999999999-0700",
		"2006-01-02 15:04:05.999999999 -0700 MST",
	}
	var lastErr error
	for _, layout := range layouts {
		t, err := time.Parse(layout, value)
		if err == nil {
			return t, nil
		}
		lastErr = err
	}
	return time.Time{}, lastErr
}

func stringValue(value any) string {
	s, _ := value.(string)
	return s
}

func optionalString(value any) *string {
	if s, ok := value.(string); ok {
		return &s
	}
	return nil
}

func stringListValue(value any) []string {
	var out []string
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
	case string:
		out = strings.Split(v, ",")
	}
	return out
}

func uintValue(value any) uint64 {
	switch v := value.(type) {
	case json.Number:
		n, err := v.Int64()
		if err == nil && n >= 0 {
			return uint64(n)
		}
		f, err := v.Float64()
		if err == nil && f >= 0 {
			return uint64(f)
		}
	case float64:
		if v >= 0 {
			return uint64(v)
		}
	}
	return 0
}

func sortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// tagsWire 对应 rustic StringList Display：排序后逗号连接。
func tagsWire(tags []string) string {
	return strings.Join(sortedUnique(tags), ",")
}

func loadSnapshotRaw(ctx context.Context, repo *repository.Repository, id restic.ID) ([]byte, error) {
	return repo.LoadUnpacked(ctx, restic.SnapshotFile, id)
}

// loadAllSnapshots 并行读全部快照。单条读不出来（旧版本写坏 / 手动改坏）按
// rustic 的行为跳过；传输层错误仍然中止，让主进程看到断连而不是空列表。
func loadAllSnapshots(ctx context.Context, repo *repository.Repository) ([]*snapshotRecord, error) {
	var (
		mu      sync.Mutex
		records []*snapshotRecord
		abort   error
	)

	err := restic.ParallelList(ctx, repo, restic.SnapshotFile, repo.Connections(), func(ctx context.Context, id restic.ID, _ int64) error {
		data, err := loadSnapshotRaw(ctx, repo, id)
		if err != nil {
			f := DescribeError(err)
			if f.Reason.isTransport() {
				mu.Lock()
				if abort == nil {
					abort = err
				}
				mu.Unlock()
				return err
			}
			return nil
		}
		rec, err := parseSnapshotRecord(id, data)
		if err != nil {
			return nil
		}
		mu.Lock()
		records = append(records, rec)
		mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, err
	}
	mu.Lock()
	defer mu.Unlock()
	if abort != nil {
		return nil, abort
	}
	// ParallelList 的完成顺序不稳定；按时间 + id 排好，让概览分组里的
	// 「取哪条 tag / description」和旧实现一样是确定的。
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Time.Equal(records[j].Time) {
			return records[i].ID.String() < records[j].ID.String()
		}
		return records[i].Time.Before(records[j].Time)
	})
	return records, nil
}

// loadSnapshotsByIDs 支持 rustic 那种完整 / 前缀 id；找不到的直接忽略。
func loadSnapshotsByIDs(ctx context.Context, repo *repository.Repository, ids []string) ([]*snapshotRecord, error) {
	var available []restic.ID
	if err := repo.List(ctx, restic.SnapshotFile, func(id restic.ID, _ int64) error {
		available = append(available, id)
		return nil
	}); err != nil {
		return nil, err
	}

	var (
		result []*snapshotRecord
		seen   = make(map[restic.ID]struct{})
	)
	for _, requested := range ids {
		requested = strings.TrimSpace(requested)
		if requested == "" {
			continue
		}
		for _, id := range available {
			if !strings.HasPrefix(id.String(), requested) {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			data, err := loadSnapshotRaw(ctx, repo, id)
			if err != nil {
				return nil, err
			}
			rec, err := parseSnapshotRecord(id, data)
			if err != nil {
				continue
			}
			seen[id] = struct{}{}
			result = append(result, rec)
		}
	}
	return result, nil
}

// findParent 复刻 rustic 默认分组（hostname + label + paths）下的最新快照。
func findParent(ctx context.Context, repo *repository.Repository, hostname, label string, paths []string) (*restic.Snapshot, error) {
	records, err := loadAllSnapshots(ctx, repo)
	if err != nil {
		return nil, err
	}

	wantPaths := sortedUnique(paths)
	var latest *snapshotRecord
	for _, rec := range records {
		if rec.Hostname != hostname || rec.Label != label {
			continue
		}
		if !equalStrings(rec.Paths, wantPaths) {
			continue
		}
		if latest == nil || rec.Time.After(latest.Time) {
			latest = rec
		}
	}
	if latest == nil {
		return nil, nil
	}
	return restic.LoadSnapshot(ctx, repo, latest.ID)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func snapshotTimeMillis(rec *snapshotRecord) int64 {
	return rec.Time.UnixMilli()
}

func backupStamp(rec *snapshotRecord) int64 {
	// rustic 的 StringList 按排序后的顺序输出，这里也先排序再取第一条带时间戳的 tag。
	for _, tag := range sortedUnique(rec.Tags) {
		if !strings.Contains(tag, "|") {
			continue
		}
		parts := strings.Split(tag, "|")
		stamp := strings.TrimSpace(parts[len(parts)-1])
		if n, err := parseI64(stamp); err == nil {
			return n
		}
	}
	return snapshotTimeMillis(rec)
}

func parseI64(text string) (int64, error) {
	var n int64
	if text == "" {
		return 0, errInvalidNumber
	}
	negative := false
	i := 0
	if text[0] == '-' {
		negative = true
		i = 1
	}
	if i == len(text) {
		return 0, errInvalidNumber
	}
	for ; i < len(text); i++ {
		c := text[i]
		if c < '0' || c > '9' {
			return 0, errInvalidNumber
		}
		n = n*10 + int64(c-'0')
	}
	if negative {
		n = -n
	}
	return n, nil
}

var errInvalidNumber = &Failure{Message: "invalid number"}

// snapshotSummary 对应 rustic 0.14 的 SnapshotSummary。字段全写，只有 error_count
// 为 0 时省略，和 serde 的 skip_serializing_if 保持一致。
type snapshotSummary struct {
	FilesNew              uint64  `json:"files_new"`
	FilesChanged          uint64  `json:"files_changed"`
	FilesUnmodified       uint64  `json:"files_unmodified"`
	TotalFilesProcessed   uint64  `json:"total_files_processed"`
	TotalBytesProcessed   uint64  `json:"total_bytes_processed"`
	DirsNew               uint64  `json:"dirs_new"`
	DirsChanged           uint64  `json:"dirs_changed"`
	DirsUnmodified        uint64  `json:"dirs_unmodified"`
	TotalDirsProcessed    uint64  `json:"total_dirs_processed"`
	TotalDirsizeProcessed uint64  `json:"total_dirsize_processed"`
	DataBlobs             uint64  `json:"data_blobs"`
	TreeBlobs             uint64  `json:"tree_blobs"`
	DataAdded             uint64  `json:"data_added"`
	DataAddedPacked       uint64  `json:"data_added_packed"`
	DataAddedFiles        uint64  `json:"data_added_files"`
	DataAddedFilesPacked  uint64  `json:"data_added_files_packed"`
	DataAddedTrees        uint64  `json:"data_added_trees"`
	DataAddedTreesPacked  uint64  `json:"data_added_trees_packed"`
	Command               string  `json:"command"`
	BackupStart           string  `json:"backup_start"`
	BackupEnd             string  `json:"backup_end"`
	BackupDuration        float64 `json:"backup_duration"`
	TotalDuration         float64 `json:"total_duration"`
	ErrorCount            uint64  `json:"error_count,omitempty"`
}

// snapshotFileJSON 是写入仓库的自定义快照。字段和 rustic SnapshotFile 对齐，
// restic / rustic 都能读。
type snapshotFileJSON struct {
	Time           string           `json:"time"`
	ProgramVersion string           `json:"program_version,omitempty"`
	Parent         *string          `json:"parent,omitempty"`
	Parents        []string         `json:"parents,omitempty"`
	Tree           string           `json:"tree"`
	Label          string           `json:"label,omitempty"`
	Paths          []string         `json:"paths"`
	Hostname       string           `json:"hostname"`
	Username       string           `json:"username"`
	UID            uint32           `json:"uid"`
	GID            uint32           `json:"gid"`
	Tags           []string         `json:"tags"`
	Original       *string          `json:"original,omitempty"`
	Summary        *snapshotSummary `json:"summary,omitempty"`
	Description    *string          `json:"description,omitempty"`
}

func formatRusticTime(t time.Time) string {
	return t.Format(time.RFC3339Nano)
}

func marshalSnapshotFile(s *snapshotFileJSON) ([]byte, error) {
	return json.Marshal(s)
}

// saveCustomSnapshot 删掉 restic archiver 自动写下的快照，再写一份带 rustic 字段
// 的快照；失败时把新写的删掉，避免仓库里留半截。
func saveCustomSnapshot(ctx context.Context, repo *repository.Repository, autoID restic.ID, snap *snapshotFileJSON) (restic.ID, error) {
	data, err := marshalSnapshotFile(snap)
	if err != nil {
		return restic.ID{}, err
	}
	newID, err := repo.SaveUnpacked(ctx, restic.WriteableSnapshotFile, data)
	if err != nil {
		_ = repo.RemoveUnpacked(ctx, restic.WriteableSnapshotFile, autoID)
		return restic.ID{}, err
	}
	if err := repo.RemoveUnpacked(ctx, restic.WriteableSnapshotFile, autoID); err != nil {
		_ = repo.RemoveUnpacked(ctx, restic.WriteableSnapshotFile, newID)
		return restic.ID{}, err
	}
	return newID, nil
}

func updateRawDescription(raw map[string]any, description *string) map[string]any {
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		out[k] = v
	}
	delete(out, "id")
	if description == nil {
		delete(out, "description")
	} else {
		out["description"] = *description
	}
	return out
}
