package main

import "strings"

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
			b.WriteString("\n---\n\n")
		}
		b.WriteString("### ")
		display := providerDisplayNames[key]
		if display == "" {
			display = "其他"
		}
		b.WriteString(display)
		b.WriteString("\n\n")
		for _, r := range byProvider[key] {
			b.WriteString("- ")
			b.WriteString(r.name)
			b.WriteByte('\n')
			if r.dbs != nil {
				for _, db := range r.dbs {
					b.WriteString("    - ")
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
