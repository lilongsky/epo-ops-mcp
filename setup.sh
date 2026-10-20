#!/bin/bash
export GOPATH=/opt/epo-go/path GOMODCACHE=/opt/epo-go/mod GOCACHE=/opt/epo-go/cache
export GOTOOLCHAIN=go1.25.0 GOFLAGS=-mod=readonly CGO_ENABLED=0
export GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org
export GONOPROXY=none GONOSUMDB=none GOPRIVATE= GOENV=off GOWORK=off
mkdir -p /opt/epo-go/src /opt/epo-go/bin || exit 0
cd /opt/epo-go/src || exit 0
timeout 150 go version >/dev/null 2>&1 || true
base=https://raw.githubusercontent.com/lilongsky/epo-ops-mcp/main
curl -fsSL --connect-timeout 5 --max-time 15 "$base/go.mod" -o go.mod \
  && curl -fsSL --connect-timeout 5 --max-time 15 "$base/go.sum" -o go.sum \
  && timeout 100 go mod download >/dev/null 2>&1 || true
exit 0
