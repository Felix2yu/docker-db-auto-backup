# docker-db-auto-backup —— 目标约定见 Felix2yu/.github/docs/makefile.md
#
# 七个必备目标：build / dev / test / lint / fmt / docker / clean
# 其余目标按仓库自身需要添加。
.PHONY: build dev test lint lint-strict fmt docker clean

BIN  := bin/db-auto-backup
MAIN := .

## 产出 $(BIN)。调度型容器：按 SCHEDULE 触发备份，不常驻 HTTP 服务
build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o $(BIN) $(MAIN)

## 手动触发一次全量备份（不等 cron）；容器内同样用 db-auto-backup 命令
dev: build
	./$(BIN)

test:
	go test ./...

## scripts/lint.sh 是仓库既有的 gofmt + vet + golangci 组合
lint:
	bash scripts/lint.sh

fmt:
	gofmt -w $$(git ls-files '*.go')

docker: build
	docker build -t docker-db-auto-backup:dev .

clean:
	rm -rf bin coverage.out
