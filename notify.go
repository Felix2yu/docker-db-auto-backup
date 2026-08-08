package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/containrrr/shoutrrr"
)

func notifyShoutrrr(ctx context.Context, urls []string, body string) {
	for _, url := range urls {
		url = strings.TrimSpace(url)
		if url == "" {
			continue
		}
		if err := shoutrrr.Send(url, body); err != nil {
			fmt.Printf("通知发送失败 (%s): %v\n", url, err)
		}
	}
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
