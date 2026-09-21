#!/usr/bin/env bash

set -e

set -x

# 允许通过 GO 环境变量指定 go 可执行文件。默认在 PATH 中查找，
# PATH 中不可用时回退到常见安装路径，避免 "command not found" 让 lint 静默失真。
GO="${GO:-}"
if [ -z "$GO" ]; then
  if command -v go >/dev/null 2>&1; then
    GO="$(command -v go)"
  else
    for cand in /opt/homebrew/bin/go /usr/local/go/bin/go /usr/local/bin/go "$HOME/go/bin/go"; do
      if [ -x "$cand" ]; then
        GO="$cand"
        break
      fi
    done
  fi
fi

if [ -z "$GO" ] || [ ! -x "$GO" ]; then
  echo "lint: 未找到 go 可执行文件，请设置 GO=/path/to/go 后重试" >&2
  exit 1
fi

# gofmt 与 go 属于同一工具链，优先复用同目录或 GOROOT/bin 下的 gofmt，
# 避免 PATH 里取不到 gofmt（或其版本与 go 不一致）导致格式检查被跳过。
GOFMT="${GOFMT:-}"
if [ -z "$GOFMT" ]; then
  GOROOT="$("$GO" env GOROOT 2>/dev/null || true)"
  for cand in "$(dirname "$GO")/gofmt" "$(dirname "$GO")/bin/gofmt" "$GOROOT/bin/gofmt"; do
    if [ -n "$cand" ] && [ -x "$cand" ]; then
      GOFMT="$cand"
      break
    fi
  done
fi

if [ -z "$GOFMT" ] || [ ! -x "$GOFMT" ]; then
  echo "lint: 未找到 gofmt 可执行文件，请设置 GOFMT=/path/to/gofmt 后重试" >&2
  exit 1
fi

# gofmt 有输出即代表存在未格式化文件，直接以非零码终止。
unformatted="$("$GOFMT" -l .)"
if [ -n "$unformatted" ]; then
  echo "lint: 以下文件未通过 gofmt，请执行 gofmt -w 后重试：" >&2
  echo "$unformatted" >&2
  exit 1
fi

"$GO" vet ./...
"$GO" test ./...
