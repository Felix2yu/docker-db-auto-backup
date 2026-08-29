package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNotifyShoutrrrEmpty(t *testing.T) {
	cfg := &config{ntfyMarkdown: true}
	notifyShoutrrr(context.Background(), cfg, nil, "body")
	notifyShoutrrr(context.Background(), cfg, []string{"  ", ""}, "body")
}

func TestNotifyShoutrrrFailure(t *testing.T) {
	cfg := &config{ntfyMarkdown: true}
	// 使用不可达地址触发发送失败分支
	notifyShoutrrr(context.Background(), cfg, []string{"http://127.0.0.1:1/does-not-exist"}, "body")
}

func TestHcPingGet(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	hcPing(srv.URL, "")
	if gotMethod != http.MethodGet {
		t.Errorf("无数据时应为 GET, got %s", gotMethod)
	}
}

func TestHcPingPost(t *testing.T) {
	var gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		b := make([]byte, 256)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	hcPing(srv.URL, "hello world")
	if gotMethod != http.MethodPost {
		t.Errorf("有数据时应为 POST, got %s", gotMethod)
	}
	if gotBody != "hello world" {
		t.Errorf("body = %q, want hello world", gotBody)
	}
}

func TestHcPingFailure(t *testing.T) {
	// 指向不可达地址，覆盖错误处理分支
	hcPing("http://127.0.0.1:1/ping", "data")
}
