package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/containrrr/shoutrrr"
)

func notifyShoutrrr(ctx context.Context, cfg *config, urls []string, body string) {
	for _, raw := range urls {
		target := strings.TrimSpace(raw)
		if target == "" {
			continue
		}
		if cfg.ntfyMarkdown && isNtfyURL(target) {
			target = enableNtfyMarkdown(target)
		}
		if err := shoutrrr.Send(target, body); err != nil {
			fmt.Printf("通知发送失败 (%s): %v\n", target, err)
		}
	}
}

func isNtfyURL(raw string) bool {
	return strings.HasPrefix(strings.ToLower(raw), "ntfy:")
}

func enableNtfyMarkdown(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := parsed.Query()
	if q.Get("markdown") == "" {
		q.Set("markdown", "yes")
		parsed.RawQuery = q.Encode()
	}
	return parsed.String()
}

func hcPing(url, data string) {
	client := &http.Client{Timeout: 10 * time.Second}
	var (
		resp *http.Response
		err  error
	)
	if data != "" {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(data))
		req.Header.Set("Content-Type", "text/plain")
		resp, err = client.Do(req)
	} else {
		resp, err = client.Get(url)
	}
	if err != nil {
		fmt.Printf("Healthchecks 心跳失败 (%s): %v\n", url, err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}
