#!/usr/bin/env python3
"""Generate plugin manifest.json from built plugins."""

import json
from pathlib import Path


def get_package_version() -> str:
    """Get package version from pyproject.toml."""
    try:
        import tomllib
    except ImportError:
        import tomli as tomllib  # type: ignore[no-redef]

    pyproject_file = Path(__file__).parent.parent / "pyproject.toml"
    with open(pyproject_file, "rb") as f:
        return tomllib.load(f)["project"]["version"]


def get_remote_plugins_config() -> tuple:
    """Get remote plugins configuration from remote_plugins.yaml.

    Returns:
        Tuple of (default_registry, plugins_list)
    """
    config_file = Path(__file__).parent.parent / "extensions" / "remote_plugins.yaml"

    if not config_file.exists():
        return None, []

    import yaml

    with open(config_file) as f:
        config = yaml.safe_load(f)
        return config.get("default_registry"), config.get("remote_plugins", [])


def _version_sort_key(s: str) -> list:
    return [(0, int(u)) if u.isdigit() else (1, u) for u in s.split(".")]


def generate_manifest() -> dict:
    """Scan plugins directory and generate manifest."""
    package_version = get_package_version()

    # Scan plugins directory for local built plugins
    plugin_dir = Path(__file__).parent.parent / "gpustack_higress_plugins" / "plugins"
    plugins = {}

    if plugin_dir.exists():
        for plugin_path in plugin_dir.iterdir():
            if plugin_path.is_dir():
                versions = []
                for v in plugin_path.iterdir():
                    if v.is_dir() and (v / "plugin.wasm").exists():
                        versions.append(v.name)
                versions.sort(key=_version_sort_key, reverse=True)
                if versions:
                    plugins[plugin_path.name] = {
                        "versions": versions,
                        "latest": versions[0],
                        "source": "local",
                    }

    # Add remote plugins from configuration
    default_registry, remote_config = get_remote_plugins_config()
    for remote_plugin in remote_config:
        name = remote_plugin["name"]
        version = remote_plugin["version"]
        source = remote_plugin["source"]
        registry = remote_plugin.get("registry")
        digest = remote_plugin.get("digest")

        # Build full source URL for manifest
        if source.startswith("oci://"):
            full_source = source
        else:
            reg = registry or default_registry or "ghcr.io/higress-extensions"
            if digest:
                # Remove tag from source if present
                source_name = source.split(":")[0] if ":" in source else source
                full_source = f"oci://{reg}/{source_name}@{digest}"
            else:
                full_source = f"oci://{reg}/{source}"

        # Check if plugin was already built (exists locally)
        if name not in plugins:
            plugin_info = {
                "versions": [version],
                "latest": version,
                "source": "remote",
                "source_url": full_source,
            }
            if digest:
                plugin_info["digest"] = digest
            plugins[name] = plugin_info
        else:
            # Plugin exists locally, add remote version as additional version
            if version not in plugins[name]["versions"]:
                plugins[name]["versions"].append(version)
                plugins[name]["versions"].sort(key=_version_sort_key, reverse=True)
            if digest and "digest" not in plugins[name]:
                plugins[name]["digest"] = digest
                plugins[name]["source_url"] = full_source

    manifest = {
        "version": package_version,
        "plugins": plugins,
    }

    return manifest


def main():
    """Generate and save manifest.json."""
    manifest = generate_manifest()

    # Save to file
    manifest_file = Path(__file__).parent.parent / "gpustack_higress_plugins" / "manifest.json"
    with open(manifest_file, "w") as f:
        json.dump(manifest, f, indent=2)

    print(f"Generated manifest: {manifest_file}")
    print(f"  Package version: {manifest['version']}")
    print(f"  Plugins found: {len(manifest['plugins'])}")

    # Count local vs remote
    local_count = sum(1 for p in manifest["plugins"].values() if p.get("source") == "local")
    remote_count = sum(1 for p in manifest["plugins"].values() if p.get("source") == "remote")

    print(f"  Local plugins: {local_count}")
    print(f"  Remote plugins: {remote_count}")

    if manifest["plugins"]:
        print("\n  Plugins:")
        for name, info in manifest["plugins"].items():
            source_str = f" ({info.get('source', 'local')})"
            print(
                f"    - {name}{source_str}: {info['latest']} (available: {', '.join(info['versions'])})"
            )
            if info.get("source_url"):
                print(f"      Source: {info['source_url']}")
            if info.get("digest"):
                digest_short = info["digest"][:20] + "..."
                print(f"      Digest: {digest_short}")


if __name__ == "__main__":
    main()
