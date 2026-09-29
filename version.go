package main

// version 由 CI 注入：-ldflags "-X main.version=v1.2.3"。
// 本地直接 go build 时保持 "dev"。
var version = "dev"
