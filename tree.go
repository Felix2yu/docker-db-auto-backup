package main

import "strings"

func formatTree(results []backupResult) string {
	var b strings.Builder
	for _, r := range results {
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
	return strings.TrimRight(b.String(), "\n")
}
