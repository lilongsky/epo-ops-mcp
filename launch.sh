#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${EPO_CLIENT_KEY:-}" || -z "${EPO_CLIENT_SECRET:-}" ]]; then
    printf '%s\n' 'EPO_CLIENT_KEY and EPO_CLIENT_SECRET must be set in the environment.' >&2
    exit 1
fi

# Keep every preparation message off the MCP protocol stream.
exec 3>&1
exec 1>&2

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
cache_root=/opt/epo-go
prepare_cache() {
    mkdir -p "$1"/path "$1"/mod "$1"/cache "$1"/bin "$1"/output 2>/dev/null &&
        [[ -w "$1/path" && -w "$1/mod" && -w "$1/cache" && -w "$1/bin" && -w "$1/output" ]]
}
if ! prepare_cache "$cache_root"; then
    cache_root="${TMPDIR:-/tmp}/epo-go"
    prepare_cache "$cache_root"
fi
export GOPATH="$cache_root/path" GOMODCACHE="$cache_root/mod" GOCACHE="$cache_root/cache"
export GOTOOLCHAIN=auto GOFLAGS=-mod=readonly CGO_ENABLED=0
export GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org
export GONOPROXY=none GONOSUMDB=none GOPRIVATE= GOENV=off GOWORK=off

# Snapshot and hash the exact inputs used for this build outside the checkout.
build_dir="$(mktemp -d "$cache_root/bin/.build.XXXXXX")"
trap 'rm -rf -- "$build_dir"' EXIT
identifiers="$(python3 - "$repo_root" "$build_dir" <<'PY'
import hashlib
import os
from pathlib import Path
import sys

repo, stage = map(Path, sys.argv[1:])
sources = [repo / 'go.mod', repo / 'go.sum']
sources += [path for path in repo.rglob('*.go') if not path.name.endswith('_test.go')]
digest = hashlib.sha256()
for source in sorted(sources, key=lambda path: path.relative_to(repo).as_posix()):
    name = source.relative_to(repo).as_posix()
    content = source.read_bytes()
    digest.update(name.encode() + b'\0' + str(len(content)).encode() + b'\0' + content)
    target = stage / name
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(content)
print(digest.hexdigest()[:12], hashlib.sha256(os.fsencode(repo)).hexdigest()[:20])
PY
)"
read -r source_hash repo_hash <<< "$identifiers"
binary="$cache_root/bin/epo-ops-mcp-$source_hash"

if [[ ! -x "$binary" ]]; then
    exec 9>"$cache_root/bin/.build-$source_hash.lock"
    flock -x 9
    if [[ ! -x "$binary" ]]; then
        printf 'Building EPO MCP (%s).\n' "$source_hash"
        (cd "$build_dir" && env -u EPO_CLIENT_KEY -u EPO_CLIENT_SECRET \
            go build -mod=readonly -trimpath -buildvcs=false -o epo-ops-mcp .)
        mv -- "$build_dir/epo-ops-mcp" "$binary"
    fi
    exec 9>&-
fi
rm -rf -- "$build_dir"
trap - EXIT

export EPO_WORKSPACE_ROOT="$cache_root/output/$repo_hash"
mkdir -p "$EPO_WORKSPACE_ROOT"
cd "$EPO_WORKSPACE_ROOT"
exec "$binary" 1>&3 3>&-
