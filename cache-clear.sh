#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DATA_DIR="${DATA_DIR:-$ROOT_DIR/data}"
if [[ ! -d "$DATA_DIR" ]]; then
  printf 'data directory not found: %s\n' "$DATA_DIR" >&2
  exit 1
fi
DATA_DIR="$(cd -- "$DATA_DIR" && pwd -P)"

if [[ ! -f "$DATA_DIR/ide.db" || -L "$DATA_DIR/ide.db" ]]; then
  printf 'database not found or is a symlink: %s/ide.db\n' "$DATA_DIR" >&2
  exit 1
fi

python3 - "$DATA_DIR/ide.db" "$DATA_DIR/public" <<'PY'
import pathlib
import re
import shutil
import sqlite3
import sys

database = pathlib.Path(sys.argv[1])
public_root = pathlib.Path(sys.argv[2])
expected_root = database.parent.resolve() / "public"

if public_root.is_symlink() or public_root.resolve() != expected_root:
    raise SystemExit("refusing to clear an unexpected public storage path")

def remove_scoped(path):
    current = public_root
    for component in path.relative_to(public_root).parts[:-1]:
        current = current / component
        if current.is_symlink():
            raise SystemExit(f"refusing to follow symlink: {current}")
    if path.is_symlink():
        path.unlink()
    elif path.is_dir():
        shutil.rmtree(path)
    elif path.exists():
        path.unlink()

connection = sqlite3.connect(database, timeout=30)
try:
    columns = {row[1] for row in connection.execute("PRAGMA table_info(builds)")}
    if not {"access_mode", "blob_owner_id"}.issubset(columns):
        raise SystemExit("build database is outdated; deploy the new server before clearing cache")

    connection.execute("BEGIN IMMEDIATE")
    active = connection.execute(
        "SELECT id FROM builds WHERE access_mode='public' "
        "AND status IN ('preparing','assembling','queued','building') LIMIT 1"
    ).fetchone()
    if active:
        connection.rollback()
        raise SystemExit(
            f"public build {active[0]} is active; wait until it finishes, then retry"
        )

    public_builds = connection.execute(
        "SELECT id,COALESCE(blob_owner_id,'') FROM builds WHERE access_mode='public'"
    ).fetchall()
    for build_id, blob_owner in public_builds:
        if not re.fullmatch(r"bld_[0-9a-f]{32}", build_id):
            raise SystemExit(f"refusing to clear invalid public build id: {build_id}")
        if blob_owner and blob_owner != f"public_build_{build_id}":
            raise SystemExit(f"refusing to clear invalid public blob owner for {build_id}")
        paths = [
            public_root / "workspaces" / build_id,
            public_root / "artifacts" / f"{build_id}.apk",
            public_root / "gradle" / build_id,
        ]
        if blob_owner:
            paths.append(public_root / "blobs" / "sha256" / blob_owner)
        for path in paths:
            remove_scoped(path)

    connection.execute(
        "DELETE FROM build_logs WHERE build_id IN "
        "(SELECT id FROM builds WHERE access_mode='public')"
    )
    connection.execute(
        "DELETE FROM build_blob_refs WHERE build_id IN "
        "(SELECT id FROM builds WHERE access_mode='public')"
    )
    connection.execute(
        "DELETE FROM blob_reservations WHERE build_id IN "
        "(SELECT id FROM builds WHERE access_mode='public')"
    )
    connection.execute(
        "DELETE FROM build_manifests WHERE build_id IN "
        "(SELECT id FROM builds WHERE access_mode='public')"
    )
    connection.execute("DELETE FROM blobs WHERE owner_id GLOB 'public_build_*'")
    connection.execute("DELETE FROM builds WHERE access_mode='public'")
    connection.commit()
    connection.execute("PRAGMA wal_checkpoint(TRUNCATE)")
    connection.execute("VACUUM")
    print(f"cleared {len(public_builds)} public build record(s) and public server storage")
except Exception:
    connection.rollback()
    raise
finally:
    connection.close()
PY
