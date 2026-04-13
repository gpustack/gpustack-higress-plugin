#!/usr/bin/env python3
"""Fetch remote OCI plugins from configuration."""

import argparse
import hashlib
import json
import os
import shutil
import subprocess
import sys
import tarfile
from datetime import datetime
from pathlib import Path

import yaml

# Import generate_manifest function directly
sys.path.insert(0, str(Path(__file__).parent))
from generate_manifest import generate_manifest


def calculate_md5(file_path: str, chunk_size: int = 4096) -> str:
    """Calculate the MD5 value of a file."""
    md5_hash = hashlib.md5()
    with open(file_path, "rb") as f:
        while chunk := f.read(chunk_size):
            md5_hash.update(chunk)
    return md5_hash.hexdigest()


def handle_tar_layer(tar_path: str, target_dir: str) -> bool:
    """Handle tar.gzip layer.

    Args:
        tar_path: Path to tar file
        target_dir: Target directory to extract to

    Returns:
        True if wasm file found and extracted, False otherwise
    """
    try:
        with tarfile.open(tar_path, "r:gz") as tar:
            wasm_files = [f for f in tar.getmembers() if f.name.endswith(".wasm")]
            if wasm_files:
                wasm_file = wasm_files[0]
                dest_path = os.path.join(target_dir, "plugin.wasm")
                if os.path.exists(dest_path):
                    os.remove(dest_path)
                with tar.extractfile(wasm_file) as src, open(dest_path, "wb") as dst:
                    dst.write(src.read())
                print(f"    Extracted .wasm from tar.gz: {dest_path}")
                return True
            else:
                print("    No .wasm file found in tar.gz")
                return False
    except Exception as e:
        print(f"    Error extracting tar file: {e}")
        return False


def handle_wasm_layer(wasm_path: str, target_dir: str) -> bool:
    """Handle .wasm layer.

    Args:
        wasm_path: Path to wasm file
        target_dir: Target directory to copy to

    Returns:
        True if wasm file was successfully copied, False otherwise
    """
    try:
        new_path = os.path.join(target_dir, "plugin.wasm")
        if os.path.exists(new_path):
            os.remove(new_path)
        shutil.copy2(wasm_path, new_path)
        print(f"    Copied .wasm file: {new_path}")
        return True
    except Exception as e:
        print(f"    Error copying .wasm file: {e}")
        return False


def generate_metadata(plugin_dir: str, plugin_name: str) -> bool:
    """Generate metadata.txt for plugin.wasm.

    Args:
        plugin_dir: Directory containing plugin.wasm
        plugin_name: Name of the plugin

    Returns:
        True if successful, False otherwise
    """
    wasm_path = os.path.join(plugin_dir, "plugin.wasm")
    try:
        stat_info = os.stat(wasm_path)
        size = stat_info.st_size
        mtime = datetime.fromtimestamp(stat_info.st_mtime).isoformat()
        ctime = datetime.fromtimestamp(stat_info.st_ctime).isoformat()
        md5_value = calculate_md5(wasm_path)
        metadata_path = os.path.join(plugin_dir, "metadata.txt")

        with open(metadata_path, "w") as f:
            f.write(f"Plugin Name: {plugin_name}\n")
            f.write(f"Size: {size} bytes\n")
            f.write(f"Last Modified: {mtime}\n")
            f.write(f"Created: {ctime}\n")
            f.write(f"MD5: {md5_value}\n")

        print(f"  ✓ Generated metadata.txt (MD5: {md5_value})")
        return True
    except Exception as e:
        print(f"  ✗ Failed to generate metadata: {e}")
        return False


def fetch_plugin(
    source: str,
    name: str,
    version: str,
    output_dir: Path,
    oras: str = "oras",
    default_registry: str = None,
    registry: str = None,
    digest: str = None,
) -> bool:
    """Fetch a single remote plugin using oras cp --to-oci-layout.

    Args:
        source: Source reference (full URL or simplified name:tag)
        name: Local plugin name
        version: Plugin version
        output_dir: Output directory for plugin
        oras: ORAS binary name
        default_registry: Default registry from config
        registry: Override registry for this plugin
        digest: Optional SHA256 digest for version pinning

    Returns:
        True if successful, False otherwise
    """
    # Build ORAS reference (without oci:// prefix)
    if source.startswith("oci://"):
        oras_ref = source[6:]  # Remove oci:// prefix
    else:
        reg = registry or default_registry or "ghcr.io/higress-extensions"
        oras_ref = f"{reg}/{source}"

    # Handle digest for ORAS reference
    if digest:
        if ":" in oras_ref.split("/")[-1]:
            parts = oras_ref.split("/")
            parts[-1] = parts[-1].split(":")[0]
            oras_ref = "/".join(parts)
        oras_ref = f"{oras_ref}@{digest}"

    print(f"Fetching: {name} version {version}")
    print(f"  From: {oras_ref}")
    if digest:
        print(f"  Digest: {digest}")

    plugin_dir = output_dir / name / version
    plugin_dir.mkdir(parents=True, exist_ok=True)

    temp_download_dir = output_dir / f"{name}_{version}_temp"
    temp_download_dir.mkdir(parents=True, exist_ok=True)

    wasm_found = False

    try:
        # Download OCI layout using oras cp
        subprocess.run(
            [oras, "cp", oras_ref, "--to-oci-layout", str(temp_download_dir)],
            check=True,
            capture_output=True,
            text=True,
        )

        # Parse index.json
        index_path = temp_download_dir / "index.json"
        with open(index_path, "r") as f:
            index = json.load(f)

        manifest_digest = index["manifests"][0]["digest"].split(":")[1]
        manifest_path = temp_download_dir / "blobs" / "sha256" / manifest_digest

        with open(manifest_path, "r") as f:
            manifest = json.load(f)

        # Process layers
        for layer in manifest.get("layers", []):
            media_type = layer.get("mediaType", "")
            digest = layer.get("digest", "").split(":")[1]
            blob_path = temp_download_dir / "blobs" / "sha256" / digest

            print(f"    Processing layer: {media_type}")

            # Handle tar.gz layers
            if media_type in [
                "application/vnd.docker.image.rootfs.diff.tar.gzip",
                "application/vnd.oci.image.layer.v1.tar+gzip",
            ]:
                if blob_path.exists():
                    if handle_tar_layer(str(blob_path), str(plugin_dir)):
                        wasm_found = True
                        break

            # Handle direct wasm layers
            elif media_type == "application/vnd.module.wasm.content.layer.v1+wasm":
                if blob_path.exists():
                    if handle_wasm_layer(str(blob_path), str(plugin_dir)):
                        wasm_found = True
                        break

    except subprocess.CalledProcessError as e:
        print(f"  ✗ ORAS command failed: {e}")
        print(f"    stderr: {e.stderr}")
        return False
    except Exception as e:
        print(f"  ✗ Error processing plugin: {e}")
        return False
    finally:
        # Clean up temp directory
        shutil.rmtree(temp_download_dir, ignore_errors=True)

    if wasm_found:
        generate_metadata(str(plugin_dir), name)
        print(f"  ✓ Successfully fetched: {name}")
    else:
        print("  ✗ No wasm file found in layers")

    return wasm_found


def main():
    parser = argparse.ArgumentParser(description="Fetch remote OCI plugins")
    parser.add_argument(
        "--config",
        default="extensions/remote_plugins.yaml",
        help="Path to remote plugins config file (used when --source is not provided)",
    )
    parser.add_argument("--oras", default="oras", help="ORAS binary name")
    parser.add_argument(
        "--output-dir",
        default=str(Path(__file__).parent.parent / "gpustack_higress_plugins" / "plugins"),
        help="Output directory for plugins",
    )
    # Single-plugin mode arguments
    parser.add_argument("--source", help="OCI source reference (single plugin mode)")
    parser.add_argument("--name", help="Local plugin name (single plugin mode)")
    parser.add_argument("--version", help="Plugin version (single plugin mode)")

    args = parser.parse_args()

    output_dir = Path(args.output_dir)

    # Single-plugin mode: fetch one plugin directly without a config file
    if args.source:
        if not args.name or not args.version:
            print("Error: --name and --version are required when --source is provided")
            sys.exit(1)
        ok = fetch_plugin(args.source, args.name, args.version, output_dir, args.oras)
        sys.exit(0 if ok else 1)

    # Config-file mode: fetch all plugins listed in the YAML config
    config_file = Path(args.config)
    if not config_file.exists():
        print(f"Error: Config file not found: {config_file}")
        sys.exit(1)

    with open(config_file) as f:
        config = yaml.safe_load(f) or {}

    default_registry = config.get("default_registry")
    if default_registry:
        print(f"Using default registry: {default_registry}")

    remote_plugins = config.get("remote_plugins", [])
    if not remote_plugins:
        print("No remote plugins configured")
        sys.exit(0)

    success_count = 0
    failed_plugins = []

    print(f"\nFetching {len(remote_plugins)} remote plugin(s)...")
    print()

    for plugin in remote_plugins:
        if fetch_plugin(
            plugin["source"],
            plugin["name"],
            plugin["version"],
            output_dir,
            args.oras,
            default_registry,
            plugin.get("registry"),
            plugin.get("digest"),
        ):
            success_count += 1
        else:
            failed_plugins.append(f"{plugin['name']}:{plugin['version']}")
        print()

    print(f"Successfully fetched {success_count}/{len(remote_plugins)} plugin(s)")

    if failed_plugins:
        print("\nFailed plugins:")
        for plugin in failed_plugins:
            print(f"  - {plugin}")

    # Regenerate manifest
    print("\nRegenerating manifest...")
    try:
        manifest = generate_manifest()
        manifest_file = Path(__file__).parent.parent / "gpustack_higress_plugins" / "manifest.json"
        with open(manifest_file, "w") as f:
            json.dump(manifest, f, indent=2)
        print(f"✓ Generated: {manifest_file}")
    except Exception as e:
        print(f"Warning: Failed to regenerate manifest: {e}")

    if success_count == len(remote_plugins):
        sys.exit(0)
    else:
        sys.exit(1)


if __name__ == "__main__":
    main()
