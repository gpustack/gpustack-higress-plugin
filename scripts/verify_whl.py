#!/usr/bin/env python3
"""Verify that a built wheel contains all expected plugins."""

import argparse
import glob
import os
import sys
import zipfile
from pathlib import Path

import yaml

ROOT = Path(__file__).parent.parent


def main() -> int:
    parser = argparse.ArgumentParser(description="Verify wheel plugin contents")
    parser.add_argument(
        "--dist-dir",
        default=str(ROOT / "dist"),
        help="Directory containing the built wheel (default: dist/)",
    )
    parser.add_argument(
        "--local-only",
        action="store_true",
        help="Only verify local plugins (skip remote_plugins.yaml entries)",
    )
    args = parser.parse_args()

    whl_files = glob.glob(os.path.join(args.dist_dir, "*.whl"))
    if not whl_files:
        print(f"✗ No wheel found in {args.dist_dir}")
        return 1
    whl_path = whl_files[0]

    # Expected: local plugins from extensions/*/VERSION
    expected: dict[str, str] = {}
    for ver_file in sorted((ROOT / "extensions").glob("*/VERSION")):
        if ver_file.parent.name.startswith("."):
            continue
        expected[ver_file.parent.name] = ver_file.read_text().strip()

    # Expected: remote plugins from remote_plugins.yaml (skipped with --local-only)
    if not args.local_only:
        config = ROOT / "extensions" / "remote_plugins.yaml"
        if config.exists():
            data = yaml.safe_load(config.read_text()) or {}
            for p in data.get("remote_plugins", []):
                expected[p["name"]] = p["version"]

    # Actual: wasm files inside wheel
    actual: dict[str, str] = {}
    has_manifest = False
    with zipfile.ZipFile(whl_path) as z:
        for f in z.namelist():
            if "manifest.json" in f:
                has_manifest = True
            if f.endswith("plugin.wasm"):
                # gpustack_higress_plugins/plugins/<name>/<version>/plugin.wasm
                parts = f.split("/")
                actual[parts[2]] = parts[3]

    size_mb = os.path.getsize(whl_path) // 1024 // 1024
    print(f"Wheel: {os.path.basename(whl_path)}  ({size_mb} MB)")
    print(f"manifest.json: {'✓' if has_manifest else '✗ MISSING'}")
    print()

    ok, missing, mismatch, extra = [], [], [], []
    for name, ver in sorted(expected.items()):
        if name not in actual:
            missing.append(f"  ✗ {name}  v{ver}  (missing)")
        elif actual[name] != ver:
            mismatch.append(f"  ✗ {name}  expected v{ver}, got v{actual[name]}")
        else:
            ok.append(f"  ✓ {name}  v{ver}")
    for name, ver in sorted(actual.items()):
        if name not in expected:
            extra.append(f"  ? {name}  v{ver}  (not in config)")

    for line in ok + extra + mismatch + missing:
        print(line)

    print()
    total = len(expected)
    print(f"Result: {len(ok)}/{total} expected plugins present", end="")
    if extra:
        print(f", {len(extra)} extra", end="")
    print()

    return 0 if (not missing and not mismatch and has_manifest) else 1


if __name__ == "__main__":
    sys.exit(main())
