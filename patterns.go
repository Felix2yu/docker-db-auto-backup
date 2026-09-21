package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// 镜像 pattern 扩展文件（B10）：让用户无需 fork 即可接入自建镜像或未被内置收录的发行版。
//
// 文件格式为 JSON：
//
//	{
//	  "patterns": [
//	    {"pattern": "bitnami/postgresql", "provider": "postgres"},
//	    {"pattern": "myorg/mydb", "provider": "mysql", "fileExt": "sql",
//	     "command": ["mysqldump", "--all-databases"]}
//	  ]
//	}
//
// 只写 pattern + provider 时，等于给内置 provider 追加一条识别规则；
// 同时给出 command 时，会注册一个使用固定命令的自定义 provider。

type imagePatternDef struct {
	Pattern  string   `json:"pattern"`
	Provider string   `json:"provider"`
	FileExt  string   `json:"fileExt,omitempty"`
	Command  []string `json:"command,omitempty"`
}

type imagePatternFile struct {
	Patterns []imagePatternDef `json:"patterns"`
}

func loadExtraProviders(file string) error {
	if strings.TrimSpace(file) == "" {
		return nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("读取镜像识别配置失败: %w", err)
	}
	var f imagePatternFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("解析镜像识别配置失败: %w", err)
	}

	var appended []*backupProvider
	for _, def := range f.Patterns {
		def.Pattern = strings.TrimSpace(def.Pattern)
		if def.Pattern == "" {
			continue
		}
		def.Provider = strings.ToLower(strings.TrimSpace(def.Provider))
		if def.Provider == "" {
			def.Provider = "custom"
		}
		if len(def.Command) > 0 {
			ext := def.FileExt
			if ext == "" {
				ext = "sql"
			}
			cmd := append([]string{}, def.Command...)
			appended = append(appended, &backupProvider{
				name:     def.Provider,
				patterns: []string{def.Pattern},
				fileExt:  ext,
				backupMethod: func(ctx context.Context, cfg *config, dc *dockerClient, containerID string) ([]string, []string, error) {
					return append([]string{}, cmd...), nil, nil
				},
			})
			continue
		}

		base := providerByName(def.Provider)
		if base == nil {
			logWarn("镜像识别配置中的 provider 不存在，已忽略", "provider", def.Provider, "pattern", def.Pattern)
			continue
		}
		clone := *base
		clone.patterns = append(append([]string{}, base.patterns...), def.Pattern)
		replaceProvider(&clone)
	}

	if len(appended) > 0 {
		providers = append(providers, appended...)
	}
	return nil
}

func providerByName(name string) *backupProvider {
	for _, p := range providers {
		if p.name == name {
			return p
		}
	}
	return nil
}

// replaceProvider 就地替换同名 provider，保持 providers 中不出现重复项。
func replaceProvider(next *backupProvider) {
	for i, p := range providers {
		if p.name == next.name {
			providers[i] = next
			return
		}
	}
	providers = append(providers, next)
}
