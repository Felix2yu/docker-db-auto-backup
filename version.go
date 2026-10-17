package main

// version 由 CI 注入：-ldflags "-X main.version=v1.2.3"。
// 本地直接 go build 时保持 "dev"。
//
//nolint:unused // 只在链接期被 -ldflags 写入，编译期没有任何引用
var version = "dev"
