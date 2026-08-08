package main

import "strings"

const dockerIndexName = "docker.io"

func resolveRepositoryName(tag string) (string, string) {
	parts := strings.Split(tag, "/")
	if len(parts) == 1 ||
		(!strings.Contains(parts[0], ".") && !strings.Contains(parts[0], ":") && parts[0] != "localhost") {
		return dockerIndexName, tag
	}
	return parts[0], strings.Join(parts[1:], "/")
}

func imageNameFromTag(tag string) string {
	registry, repo := resolveRepositoryName(tag)
	if registry == dockerIndexName {
		repo = strings.TrimPrefix(repo, "library/")
	}
	if idx := strings.Index(repo, ":"); idx >= 0 {
		repo = repo[:idx]
	}
	return repo
}

func imageNamesFromTags(tags []string) []string {
	var names []string
	seen := map[string]struct{}{}
	for _, tag := range tags {
		name := imageNameFromTag(tag)
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}
