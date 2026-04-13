#!/usr/bin/env python3
"""Generate metadata.txt for a plugin.wasm file."""

import argparse
import hashlib
import os
from datetime import datetime


def calculate_md5(file_path: str) -> str:
    """Calculate MD5 checksum of a file."""
    md5_hash = hashlib.md5()
    with open(file_path, "rb") as f:
        for chunk in iter(lambda: f.read(8192), b""):
            md5_hash.update(chunk)
    return md5_hash.hexdigest()


def generate_metadata(wasm_path: str, plugin_name: str) -> bool:
    """Generate metadata.txt for plugin.wasm.

    Args:
        wasm_path: Path to plugin.wasm file
        plugin_name: Name of the plugin

    Returns:
        True if successful, False otherwise
    """
    if not os.path.exists(wasm_path):
        print(f"Error: plugin.wasm not found at {wasm_path}")
        return False

    try:
        stat_info = os.stat(wasm_path)
        size = stat_info.st_size
        mtime = datetime.fromtimestamp(stat_info.st_mtime).isoformat()
        ctime = datetime.fromtimestamp(stat_info.st_ctime).isoformat()
        md5_value = calculate_md5(wasm_path)

        plugin_dir = os.path.dirname(wasm_path)
        metadata_path = os.path.join(plugin_dir, "metadata.txt")

        with open(metadata_path, "w") as f:
            f.write(f"Plugin Name: {plugin_name}\n")
            f.write(f"Size: {size} bytes\n")
            f.write(f"Last Modified: {mtime}\n")
            f.write(f"Created: {ctime}\n")
            f.write(f"MD5: {md5_value}\n")

        print(f"✓ Generated metadata.txt: {metadata_path}")
        print(f"  Plugin: {plugin_name}")
        print(f"  Size: {size} bytes")
        print(f"  MD5: {md5_value}")
        return True

    except Exception as e:
        print(f"✗ Failed to generate metadata: {e}")
        return False


def main():
    parser = argparse.ArgumentParser(description="Generate metadata.txt for plugin.wasm")
    parser.add_argument("wasm_path", help="Path to plugin.wasm file")
    parser.add_argument("plugin_name", help="Name of the plugin")
    args = parser.parse_args()

    if generate_metadata(args.wasm_path, args.plugin_name):
        exit(0)
    else:
        exit(1)


if __name__ == "__main__":
    main()
