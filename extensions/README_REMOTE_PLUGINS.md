# Remote OCI Plugins

This document describes how to configure and fetch remote OCI plugins from external registries.

## Overview

Remote plugins allow you to include third-party Higress plugins from OCI registries (like Docker Hub, GitHub Container Registry, etc.) alongside your locally-built plugins.

## Configuration

### 1. Create/Edit `remote_plugins.yaml`

Create or edit `extensions/remote_plugins.yaml`:

```yaml
remote_plugins:
  # Format for each plugin:
  # - source: oci://registry/org/plugin:tag
  #   name: local-plugin-name
  #   version: plugin-version

  # Example: Higress official AI Proxy plugin
  - source: oci://ghcr.io/higress-extensions/ai-proxy:1.2.3
    name: ai-proxy
    version: 1.2.3

  # Example: JWT Auth plugin
  - source: oci://ghcr.io/higress-extensions/jwt-auth:1.1.0
    name: jwt-auth
    version: 1.1.0
```

### 2. Install ORAS

ORAS is required to fetch OCI artifacts:

```bash
# macOS
brew install oras

# Linux
curl -LO https://github.com/oras-project/oras/releases/download/v1.2.3/oras_1.2.3_linux_amd64.tar.gz
mkdir -p /usr/local/bin
tar -zxf oras_1.2.3_linux_amd64.tar.gz -C /usr/local/bin oras
rm oras_1.2.3_linux_amd64.tar.gz
```

### 3. Install Python Dependencies

```bash
make dev
# or
uv pip install pyyaml
```

## Usage

### Fetch All Remote Plugins

```bash
# From project root
make -C extensions fetch-all-remote
```

### Build Local + Fetch Remote

```bash
# Build all local plugins and fetch remote plugins
make -C extensions build-all-with-remote
```

### Fetch Single Remote Plugin

```bash
# Fetch a specific plugin manually
make -C extensions fetch-remote PLUGIN='oci://ghcr.io/higress-extensions/ai-proxy:1.2.3|ai-proxy|1.2.3'
```

### List Available Plugins

```bash
# List all plugins (local + remote)
make plugins

# Or check manifest directly
cat gpustack_higress_plugins/manifest.json
```

## Plugin Directory Structure

After fetching, remote plugins are stored alongside local plugins:

```
gpustack_higress_plugins/plugins/
├── gpustack-token-usage/        # Local plugin
│   └── 1.0.0/
│       └── plugin.wasm
├── ai-proxy/                      # Remote plugin
│   └── 1.2.3/
│       └── plugin.wasm
└── jwt-auth/                      # Remote plugin
    └── 1.1.0/
        └── plugin.wasm
```

## Higress Configuration

Configure Higress WasmPlugin to use remote plugins:

```yaml
apiVersion: extensions.higress.io/v1alpha1
kind: WasmPlugin
metadata:
  name: ai-proxy
  namespace: higress-system
spec:
  url: http://plugin-server:8080/ai-proxy/1.2.3/plugin.wasm
  # Plugin-specific configuration
  defaultConfig:
    # ... config ...
```

## Troubleshooting

### ORAS Command Failed

**Error**: `oras: command not found`

**Fix**: Install ORAS:
```bash
brew install oras  # macOS
```

### Permission Denied

**Error**: Failed to pull from registry

**Fix**: Login to the registry:
```bash
oras login ghcr.io
```

### Plugin Not Found After Fetch

**Check**: Verify the plugin was fetched:
```bash
ls -la gpustack_higress_plugins/plugins/
```

**Fix**: Re-run fetch-all-remote and check error messages

### PyYAML Import Error

**Error**: `No module named 'yaml'`

**Fix**: Install PyYAML:
```bash
uv pip install pyyaml
# or
make install-dev
```

## Examples

### Example 1: Official Higress Plugins

```yaml
remote_plugins:
  - source: oci://ghcr.io/higress-extensions/ai-proxy:1.2.3
    name: ai-proxy
    version: 1.2.3

  - source: oci://ghcr.io/higress-extensions/jwt-auth:1.1.0
    name: jwt-auth
    version: 1.1.0
```

### Example 2: Custom Registry

```yaml
remote_plugins:
  - source: oci://docker.io/myorg/higress-plugin:2.0.0
    name: my-custom-plugin
    version: 2.0.0
```

### Example 3: Multiple Versions

You can fetch multiple versions of the same plugin:

```yaml
remote_plugins:
  - source: oci://ghcr.io/higress-extensions/ai-proxy:1.2.3
    name: ai-proxy
    version: 1.2.3

  - source: oci://ghcr.io/higress-extensions/ai-proxy:1.3.0
    name: ai-proxy
    version: 1.3.0
```

The manifest will track all available versions.

## Best Practices

1. **Version Pinning**: Always pin specific versions in `remote_plugins.yaml`
2. **Test Before Deploy**: Test remote plugins in development before production
3. **Registry Authentication**: Use private registries with proper authentication
4. **Verify Checksums**: Verify plugin integrity after fetching
5. **Update Manifest**: Always regenerate manifest after fetching plugins
