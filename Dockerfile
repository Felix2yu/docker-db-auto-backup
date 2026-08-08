FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/db-auto-backup .

FROM alpine:3.21

ENV SCHEDULE="0 0 * * *" TZ=Asia/Shanghai

RUN apk add --no-cache tzdata ca-certificates \
 && mkdir -p /var/backups

COPY --from=build /out/db-auto-backup /usr/local/bin/db-auto-backup

CMD ["db-auto-backup"]