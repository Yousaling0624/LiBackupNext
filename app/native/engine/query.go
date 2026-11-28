package engine

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/restic/restic/internal/restic"
)

// ListSnapshots 按 label 过滤快照。应用按 `<包名>:` 前缀，绝对路径目录按
// `<路径>:dir` 精确匹配（避免 /a/DCIM 命中 /a/DCIM2）。
func ListSnapshots(packageName string) ([]SnapshotInfo, error) {
	repo, err := openRepo()
	if err != nil {
		return nil, err
	}
	defer repo.release()
	records, err := loadAllSnapshots(context.Background(), repo.Repository)
	if err != nil {
		return nil, wrapFailure("", err)
	}

	prefix := packageName + ":"
	var dirLabel string
	if strings.HasPrefix(packageName, "/") {
		dirLabel = packageName + ":dir"
	}
	filterByPackage := packageName != ""

	infos := make([]SnapshotInfo, 0, len(records))
	for _, rec := range records {
		if dirLabel != "" {
			if rec.Label != dirLabel {
				continue
			}
		} else if filterByPackage && !strings.HasPrefix(rec.Label, prefix) {
			continue
		}
		infos = append(infos, SnapshotInfo{
			ID:          rec.ID.String(),
			Label:       rec.Label,
			Tags:        tagsWire(rec.Tags),
			Time:        snapshotTimeMillis(rec),
			Size:        rec.Size,
			Description: rec.Description,
		})
	}

	sort.SliceStable(infos, func(i, j int) bool { return infos[i].Time > infos[j].Time })
	limit := 2000
	if filterByPackage {
		limit = 500
	}
	if len(infos) > limit {
		infos = infos[:limit]
	}
	return infos, nil
}

// Overview 每个包一条：最近一次备份的快照 + 最新 apk 快照 + 是否有图标。
func Overview() ([]PackageOverview, error) {
	repo, err := openRepo()
	if err != nil {
		return nil, err
	}
	defer repo.release()
	records, err := loadAllSnapshots(context.Background(), repo.Repository)
	if err != nil {
		return nil, wrapFailure("", err)
	}

	byPackage := make(map[string][]*snapshotRecord)
	iconPackages := make(map[string]bool)
	for _, rec := range records {
		index := strings.LastIndexByte(rec.Label, ':')
		if index <= 0 {
			continue
		}
		pkg := rec.Label[:index]
		kind := rec.Label[index+1:]
		if pkg == "" {
			continue
		}
		if kind == "icon" {
			iconPackages[pkg] = true
			continue
		}
		byPackage[pkg] = append(byPackage[pkg], rec)
	}

	result := make([]PackageOverview, 0, len(byPackage))
	for pkg, group := range byPackage {
		byStamp := make(map[int64][]*snapshotRecord)
		for _, rec := range group {
			stamp := backupStamp(rec)
			byStamp[stamp] = append(byStamp[stamp], rec)
		}
		if len(byStamp) == 0 {
			continue
		}

		var latestStamp int64
		first := true
		for stamp := range byStamp {
			if first || stamp > latestStamp {
				latestStamp = stamp
				first = false
			}
		}
		latest := byStamp[latestStamp]

		tags := ""
		if len(latest) > 0 {
			tags = tagsWire(latest[0].Tags)
		}
		var description *string
		for _, rec := range latest {
			if rec.Description != nil && strings.TrimSpace(*rec.Description) != "" {
				description = rec.Description
				break
			}
		}

		snapshots := make([]OverviewSnapshot, 0, len(latest))
		for _, rec := range latest {
			snapshots = append(snapshots, OverviewSnapshot{
				ID:    rec.ID.String(),
				Label: rec.Label,
				Size:  rec.Size,
			})
		}

		var apk *snapshotRecord
		var apkStamp int64
		for _, rec := range group {
			if !strings.HasSuffix(rec.Label, ":apk") {
				continue
			}
			stamp := backupStamp(rec)
			if apk == nil || stamp >= apkStamp {
				apk = rec
				apkStamp = stamp
			}
		}
		var apkTags *string
		if apk != nil {
			value := tagsWire(apk.Tags)
			apkTags = &value
		}

		result = append(result, PackageOverview{
			Package:     pkg,
			Count:       uint32(len(byStamp)),
			Time:        latestStamp,
			Tags:        tags,
			Description: description,
			Snapshots:   snapshots,
			HasApk:      apk != nil,
			ApkTags:     apkTags,
			ApkTime:     apkStamp,
			HasIcon:     iconPackages[pkg],
		})
	}

	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Time == result[j].Time {
			return result[i].Package < result[j].Package
		}
		return result[i].Time > result[j].Time
	})
	return result, nil
}

// UpdateDescriptions 用原始 JSON 改 description 后另存，再删旧 id。
func UpdateDescriptions(ids []string, description *string) error {
	leave := enterWriteGate()
	defer leave()

	if len(ids) == 0 {
		return nil
	}
	repo, err := openRepo()
	if err != nil {
		return err
	}
	defer repo.release()
	records, err := loadSnapshotsByIDs(context.Background(), repo.Repository, ids)
	if err != nil {
		return wrapFailure("", err)
	}
	if len(records) == 0 {
		return appFailure("snapshot not found")
	}

	if description != nil && strings.TrimSpace(*description) == "" {
		description = nil
	}

	ctx := context.Background()
	oldIDs := make([]restic.ID, 0, len(records))
	for _, rec := range records {
		oldIDs = append(oldIDs, rec.ID)
		raw := updateRawDescription(rec.Raw, description)
		data, err := json.Marshal(raw)
		if err != nil {
			return wrapFailure("", err)
		}
		if _, err := repo.SaveUnpacked(ctx, restic.WriteableSnapshotFile, data); err != nil {
			invalidateIndexed()
			return wrapFailure("", err)
		}
	}
	for _, id := range oldIDs {
		if err := repo.RemoveUnpacked(ctx, restic.WriteableSnapshotFile, id); err != nil {
			invalidateIndexed()
			return wrapFailure("", err)
		}
	}
	invalidateIndexed()
	return nil
}

func DeleteSnapshots(ids []string) error {
	leave := enterWriteGate()
	defer leave()

	if len(ids) == 0 {
		return nil
	}
	invalidateIndexed()
	defer invalidateIndexed()

	repo, err := openRepo()
	if err != nil {
		return err
	}
	defer repo.release()
	records, err := loadSnapshotsByIDs(context.Background(), repo.Repository, ids)
	if err != nil {
		return wrapFailure("", err)
	}
	for _, rec := range records {
		if err := repo.RemoveUnpacked(context.Background(), restic.WriteableSnapshotFile, rec.ID); err != nil {
			return wrapFailure("", err)
		}
	}
	return nil
}

// CleanOrphanIcons 删掉包已经没有任何真实备份的 `包名:icon` 快照。
func CleanOrphanIcons() (uint32, error) {
	leave := enterWriteGate()
	defer leave()

	repo, err := openRepo()
	if err != nil {
		return 0, err
	}
	defer repo.release()
	records, err := loadAllSnapshots(context.Background(), repo.Repository)
	if err != nil {
		return 0, wrapFailure("", err)
	}

	hasBackup := make(map[string]bool)
	icons := make(map[string][]*snapshotRecord)
	for _, rec := range records {
		index := strings.LastIndexByte(rec.Label, ':')
		if index <= 0 {
			continue
		}
		pkg := rec.Label[:index]
		kind := rec.Label[index+1:]
		if pkg == "" {
			continue
		}
		if kind == "icon" {
			icons[pkg] = append(icons[pkg], rec)
		} else {
			hasBackup[pkg] = true
		}
	}

	var orphanIDs []restic.ID
	for pkg, group := range icons {
		if hasBackup[pkg] {
			continue
		}
		for _, rec := range group {
			orphanIDs = append(orphanIDs, rec.ID)
		}
	}
	if len(orphanIDs) == 0 {
		return 0, nil
	}

	for _, id := range orphanIDs {
		if err := repo.RemoveUnpacked(context.Background(), restic.WriteableSnapshotFile, id); err != nil {
			return 0, wrapFailure("", err)
		}
	}
	invalidateIndexed()
	return uint32(len(orphanIDs)), nil
}
