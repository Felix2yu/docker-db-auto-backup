# 运行时镜像：主程序由 CI 预编译并下载到 bin/ 后拼装。
# 保留 kopia 阶段——它需要按 TARGETARCH 从上游下载对应架构的二进制。
FROM alpine:3.24 AS kopia

ARG TARGETARCH
ARG KOPIA_VERSION=0.23.1

RUN apk add --no-cache curl \
 && case "$TARGETARCH" in \
      amd64) arch=x64 ;; \
      arm64) arch=arm64 ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac \
 && curl -fsSL -o /tmp/kopia.tar.gz \
      "https://github.com/kopia/kopia/releases/download/v${KOPIA_VERSION}/kopia-${KOPIA_VERSION}-linux-${arch}.tar.gz" \
 && mkdir -p /out \
 && tar -xzf /tmp/kopia.tar.gz -C /tmp "kopia-${KOPIA_VERSION}-linux-${arch}/kopia" \
 && install -m 0755 "/tmp/kopia-${KOPIA_VERSION}-linux-${arch}/kopia" /out/kopia \
 && rm -f /tmp/kopia.tar.gz

FROM alpine:3.24

ENV SCHEDULE="0 0 * * *" TZ=Asia/Shanghai

RUN apk add --no-cache tzdata ca-certificates \
 && mkdir -p /var/backups

COPY --chmod=755 bin/db-auto-backup /usr/local/bin/db-auto-backup
COPY --from=kopia /out/kopia /usr/local/bin/kopia

CMD ["db-auto-backup"]
