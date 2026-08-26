FROM golang:1.27-alpine AS build

ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=$GOPROXY

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/db-auto-backup .

FROM alpine:3.21 AS kopia

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

FROM alpine:3.21

ENV SCHEDULE="0 0 * * *" TZ=Asia/Shanghai

RUN apk add --no-cache tzdata ca-certificates \
 && mkdir -p /var/backups

COPY --from=build /out/db-auto-backup /usr/local/bin/db-auto-backup
COPY --from=kopia /out/kopia /usr/local/bin/kopia

CMD ["db-auto-backup"]