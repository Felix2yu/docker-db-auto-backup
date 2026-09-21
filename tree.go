package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

var providerOrder = []string{"postgres", "mysql", "redis"}

var providerDisplayNames = map[string]string{
	"postgres": "PostgreSQL",
	"mysql":    "MySQL",
	"redis":    "Redis",
}

func formatTree(results []backupResult) string {
	if len(results) == 0 {
		return ""
	}

	byProvider := map[string][]backupResult{}
	for _, r := range results {
		key := r.providerType
		if key == "" {
			key = "other"
		}
		byProvider[key] = append(byProvider[key], r)
	}

	var groupOrder []string
	for _, key := range providerOrder {
		if _, ok := byProvider[key]; ok {
			groupOrder = append(groupOrder, key)
		}
	}
	for key := range byProvider {
		if key == "other" {
			groupOrder = append(groupOrder, key)
		}
	}

	var b strings.Builder
	for i, key := range groupOrder {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("### ")
		display := providerDisplayNames[key]
		if display == "" {
			display = "其他"
		}
		b.WriteString(display)
		b.WriteByte('\n')
		for _, r := range byProvider[key] {
			b.WriteString("- ")
			b.WriteString(r.name)
			b.WriteByte('\n')
			if r.dbs != nil {
				for _, db := range r.dbs {
					b.WriteString("  - ")
					if db.isSystem {
						b.WriteString("*")
						b.WriteString(db.name)
						b.WriteString("*")
					} else {
						b.WriteString(db.name)
					}
					b.WriteByte('\n')
				}
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatManifestTree 渲染已备份容器树，附带每个库的体积（A7）。
func formatManifestTree(m *backupManifest) string {
	if m == nil || len(m.Containers) == 0 {
		return ""
	}

	byProvider := map[string][]containerManifest{}
	for _, c := range m.Containers {
		key := c.Provider
		if key == "" {
			key = "other"
		}
		byProvider[key] = append(byProvider[key], c)
	}

	var groupOrder []string
	for _, key := range providerOrder {
		if _, ok := byProvider[key]; ok {
			groupOrder = append(groupOrder, key)
		}
	}
	for key := range byProvider {
		if _, known := providerDisplayNames[key]; !known {
			groupOrder = append(groupOrder, key)
		}
	}

	var b strings.Builder
	for i, key := range groupOrder {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("### ")
		display := providerDisplayNames[key]
		if display == "" {
			display = "其他"
		}
		b.WriteString(display)
		b.WriteByte('\n')
		for _, c := range byProvider[key] {
			b.WriteString("- ")
			b.WriteString(c.Name)
			switch c.Mode {
			case modeFullFallback:
				b.WriteString("（单库模式回退为全库备份）")
			case modeFull:
				b.WriteString("（全库）")
			}
			b.WriteString(fmt.Sprintf(" — %s", humanBytes(sizeOf(c))))
			b.WriteByte('\n')
			for _, f := range c.Files {
				b.WriteString("  - ")
				name := f.Database
				if name == "" {
					name = filepath.Base(f.Path)
				}
				if f.System {
					name = "*" + name + "*"
				}
				b.WriteString(fmt.Sprintf("%s (%s)", name, humanBytes(f.Size)))
				b.WriteByte('\n')
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func sizeOf(c containerManifest) int64 {
	var total int64
	for _, f := range c.Files {
		total += f.Size
	}
	return total
}

// formatReport 生成通知正文（A7）：除容器清单外，补充体积、耗时、失败明细与异地快照信息。
func formatReport(m *backupManifest, durationStr string) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("**备份%s**\n\n", statusLabel(m.Status)))
	b.WriteString(fmt.Sprintf("- 日期：%s\n", m.Date))
	b.WriteString(fmt.Sprintf("- 耗时：%s\n", durationStr))
	b.WriteString(fmt.Sprintf("- 容器：%d 个\n", len(m.Containers)))
	b.WriteString(fmt.Sprintf("- 文件：%d 个 / %s\n", m.fileCount(), humanBytes(m.totalBytes())))
	if m.Kopia != nil {
		if m.Kopia.Pushed {
			b.WriteString(fmt.Sprintf("- 异地快照：已推送（%s）\n", orDefault(m.Kopia.SnapshotID, "无 ID")))
		} else {
			b.WriteString(fmt.Sprintf("- 异地快照：失败（%s）\n", orDefault(m.Kopia.Error, "未知原因")))
		}
	}

	if tree := formatManifestTree(m); tree != "" {
		b.WriteString("\n**已备份容器：**\n\n")
		b.WriteString(tree)
		b.WriteString("\n")
	}

	if len(m.Failures) > 0 {
		b.WriteString("\n**失败明细：**\n\n")
		for _, f := range m.Failures {
			b.WriteString("- ")
			b.WriteString(f)
			b.WriteByte('\n')
		}
	}
	if len(m.Warnings) > 0 {
		b.WriteString("\n**警告：**\n\n")
		for _, w := range m.Warnings {
			b.WriteString("- ")
			b.WriteString(w)
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
