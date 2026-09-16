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

    # Expected: local plugins from extensions/*/VERSION.
    #
    # A .nopackage marker in the plugin directory means the source is kept but
    # the wasm is deliberately not shipped, so it must not be expected here --
    # extensions/Makefile's build-all skips the same marker. Keep the two in
    # step; the marker file is the single representation of that fact.
    expected: dict[str, str] = {}
    excluded: list[str] = []
    for ver_file in sorted((ROOT / "extensions").glob("*/VERSION")):
        plugin_dir = ver_file.parent
        if plugin_dir.name.startswith("."):
            continue
        if (plugin_dir / ".nopackage").exists():
            excluded.append(plugin_dir.name)
            continue
        expected[plugin_dir.name] = ver_file.read_text().strip()

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

    ok, missing, mismatch, extra, leaked = [], [], [], [], []
    for name, ver in sorted(expected.items()):
        if name not in actual:
            missing.append(f"  ✗ {name}  v{ver}  (missing)")
        elif actual[name] != ver:
            mismatch.append(f"  ✗ {name}  expected v{ver}, got v{actual[name]}")
        else:
            ok.append(f"  ✓ {name}  v{ver}")
    for name, ver in sorted(actual.items()):
        if name in excluded:
            # A .nopackage plugin that shipped anyway. The usual cause is a
            # stale gpustack_higress_plugins/plugins/ from an earlier build
            # (force-include packages whatever is in that directory), so this
            # has to fail rather than warn -- the whole point of the marker is
            # to keep these bytes out of the wheel.
            leaked.append(f"  ✗ {name}  v{ver}  (marked .nopackage; run make clean)")
        elif name not in expected:
            extra.append(f"  ? {name}  v{ver}  (not in config)")

    for line in ok + extra + mismatch + missing + leaked:
        print(line)

    if excluded:
        print()
        print(f"Excluded by .nopackage: {', '.join(sorted(excluded))}")

    print()
    total = len(expected)
    print(f"Result: {len(ok)}/{total} expected plugins present", end="")
    if extra:
        print(f", {len(extra)} extra", end="")
    if leaked:
        print(f", {len(leaked)} excluded-but-present", end="")
    print()

    return 0 if (not missing and not mismatch and not leaked and has_manifest) else 1


if __name__ == "__main__":
    sys.exit(main())
