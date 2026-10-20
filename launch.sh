#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${EPO_CLIENT_KEY:-}" || -z "${EPO_CLIENT_SECRET:-}" ]]; then
    printf '%s\n' 'EPO_CLIENT_KEY and EPO_CLIENT_SECRET must be set in the environment.' >&2
    exit 1
fi

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
# Preserve the client's stdio input while Python reads this launcher program.
exec python3 - "$repo_root" 3<&0 <<'PY'
import hashlib
import os
from pathlib import Path
import sys

repo = Path(sys.argv[1]).resolve()
repo_id = hashlib.sha256(os.fsencode(repo)).hexdigest()[:20]
runtime = Path("/tmp") / f"epo-ops-mcp-{os.getuid()}-{repo_id}"
if runtime.is_symlink():
    sys.exit("The external runtime directory must not be a symlink.")
runtime = runtime.resolve()
if runtime == repo or repo in runtime.parents:
    sys.exit("The runtime directory must be outside the repository.")
binary = runtime / "bin" / "epo-ops-mcp"
if not binary.is_file() or not os.access(binary, os.X_OK):
    sys.exit("The MCP binary is missing; run ./setup.sh successfully before launching.")
output = runtime / "output"
if output.is_symlink():
    sys.exit("The output directory must not be a symlink.")
output.mkdir(mode=0o700, parents=True, exist_ok=True)
os.environ["EPO_WORKSPACE_ROOT"] = str(output)
os.chdir(output)
os.dup2(3, 0)
os.close(3)
os.execv(str(binary), [str(binary)])
PY
