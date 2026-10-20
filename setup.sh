#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
exec python3 - "$repo_root" <<'PY'
import hashlib
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import time

started = time.monotonic()
deadline = started + 280
repo = Path(sys.argv[1]).resolve()
repo_id = hashlib.sha256(os.fsencode(repo)).hexdigest()[:20]
runtime = Path("/tmp") / f"epo-ops-mcp-{os.getuid()}-{repo_id}"
stage = None


def run(command, **kwargs):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise subprocess.TimeoutExpired(command, 280)
    return subprocess.run(command, check=True, timeout=remaining, **kwargs)


try:
    if runtime.is_symlink():
        raise RuntimeError("The external runtime directory must not be a symlink.")
    runtime.mkdir(mode=0o700, parents=True, exist_ok=True)
    runtime = runtime.resolve()
    if runtime == repo or repo in runtime.parents:
        raise RuntimeError("The runtime directory must be outside the repository.")
    for name in ("bin", "build", "gocache", "gomodcache", "gopath", "tmp", "output"):
        directory = runtime / name
        if directory.is_symlink():
            raise RuntimeError("Runtime subdirectories must not be symlinks.")
        directory.mkdir(mode=0o700, exist_ok=True)

    environment = os.environ.copy()
    environment.update({
        "GOTOOLCHAIN": "local",
        "GOPROXY": "https://proxy.golang.org",
        "GOSUMDB": "sum.golang.org",
        "GONOPROXY": "none",
        "GONOSUMDB": "none",
        "GOPRIVATE": "",
        "GOENV": "off",
        "GOWORK": "off",
        "GOFLAGS": "-mod=readonly",
        "CGO_ENABLED": "0",
        "GOCACHE": str(runtime / "gocache"),
        "GOMODCACHE": str(runtime / "gomodcache"),
        "GOPATH": str(runtime / "gopath"),
        "GOTMPDIR": str(runtime / "tmp"),
        "TMPDIR": str(runtime / "tmp"),
        "TMP": str(runtime / "tmp"),
        "TEMP": str(runtime / "tmp"),
    })
    for name in ("GOOS", "GOARCH", "GOEXPERIMENT", "EPO_CLIENT_KEY", "EPO_CLIENT_SECRET"):
        environment.pop(name, None)

    version = run(["go", "env", "GOVERSION"], cwd=runtime, env=environment,
                  stdout=subprocess.PIPE, text=True).stdout.strip()
    match = re.fullmatch(r"go(\d+)\.(\d+)(?:\.(\d+))?", version)
    if not match or tuple(int(part or 0) for part in match.groups()) < (1, 25, 0):
        raise RuntimeError("Preinstalled Go 1.25.0 or later is required; setup does not download Go.")

    stage = Path(tempfile.mkdtemp(prefix="source-", dir=runtime / "build"))
    sources = sorted(path for path in repo.glob("*.go") if not path.name.endswith("_test.go"))
    if not sources:
        raise RuntimeError("No Go source files were found at the repository root.")
    for source in [repo / "go.mod", repo / "go.sum", *sources]:
        shutil.copyfile(source, stage / source.name)
    locked = {name: (stage / name).read_bytes() for name in ("go.mod", "go.sum")}

    run(["go", "mod", "download"], cwd=stage, env=environment)
    if any((stage / name).read_bytes() != content for name, content in locked.items()):
        raise RuntimeError("Dependency installation changed a lock file; update the source locks before setup.")
    run(["go", "mod", "verify"], cwd=stage, env=environment)
    built = stage / "epo-ops-mcp"
    run(["go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-o", str(built), "."],
        cwd=stage, env=environment)
    os.replace(built, runtime / "bin" / "epo-ops-mcp")
    print(f"Setup completed in {time.monotonic() - started:.1f}s using {version}.")
    print(f"Runtime and output directory: {runtime}")
except subprocess.TimeoutExpired:
    print("Setup exceeded its 280-second budget.", file=sys.stderr)
    sys.exit(1)
except subprocess.CalledProcessError as error:
    print(f"Setup command failed with exit code {error.returncode}.", file=sys.stderr)
    sys.exit(1)
except (OSError, RuntimeError) as error:
    print(f"Setup failed: {error}", file=sys.stderr)
    sys.exit(1)
finally:
    if stage is not None:
        shutil.rmtree(stage, ignore_errors=True)
PY
