#!/usr/bin/env bash

set -e

set -x

gofmt -l .
go vet ./...
go test ./...