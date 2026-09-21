package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	apprise "github.com/unraid/apprise-go"
)

func notify(ctx context.Context, cfg *config, urls []string, body string) {
	for _, raw := range urls {
		target := strings.TrimSpace(raw)
		if target == "" {
			continue
		}
		if cfg.ntfyMarkdown && isNtfyURL(target) {
			target = ensureNtfyMarkdownFormat(target)
		}
		if err := apprise.Send([]string{target}, body, apprise.WithInputFormat("markdown")); err != nil {
			fmt.Printf("通知发送失败 (%s): %v\n", target, err)
		}
	}
}

func isNtfyURL(raw string) bool {
	return strings.HasPrefix(strings.ToLower(raw), "ntfy:")
}

// ensureNtfyMarkdownFormat makes apprise-go send the message to ntfy with the
// markdown format so ntfy actually renders it.
//
// apprise-go's ntfy handler only honours the ?format= query parameter to decide
// the message format: it sets ntfy's "X-Markdown: yes" header from it
// (internal/notify/ntfy.go). The commonly used ?markdown=yes is silently
// ignored, so the message arrives as plain text. Worse, with no format set the
// ntfy default output format is "text", which makes apprise convert the body
// markdown -> HTML -> text, stripping headings and mangling blank lines.
func ensureNtfyMarkdownFormat(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := parsed.Query()
	if q.Get("format") == "" {
		q.Set("format", "markdown")
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
