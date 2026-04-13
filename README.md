# GPUStack Higress Plugins

Higress Proxy-Wasm plugins for GPUStack, providing AI API traffic processing, observability, and enhanced gateway features.

## Overview

This repository contains custom Higress Proxy-Wasm plugins designed for GPUStack, distributed as a Python package that includes pre-compiled Wasm plugins and a built-in HTTP file server for serving them.

## Installation

```bash
pip install gpustack-higress-plugins
```

**Requirements**: Python >= 3.10

## Available Plugins

- **gpustack-token-usage** - Collects and injects token usage statistics into AI API responses. For streaming responses: time to first token, time per output token, and tokens per second. For non-streaming responses: tokens per second only. Supports real client IP injection and path-based filtering.

- **gpustack-set-header-pre-route** - Automatically injects the route name and model name into HTTP request headers before routing, based on configurable path suffixes or prefixes.

## Usage

### Start Plugin Server

```bash
# Start the built-in HTTP file server
gpustack-plugins start --port 8080

# Or with custom host
gpustack-plugins start --port 8080 --host 0.0.0.0
```

The server will be available at `http://localhost:8080`.

### API Endpoints

```bash
# Health check
curl http://localhost:8080/

# Download a plugin
curl http://localhost:8080/wasm-plugins/gpustack-token-usage/1.0.0/plugin.wasm -o plugin.wasm

# Get metadata
curl http://localhost:8080/wasm-plugins/gpustack-token-usage/1.0.0/metadata.txt
```

### Python API

```python
from gpustack_higress_plugins import create_app, router

# Embed in an existing FastAPI app
app.include_router(router)

# Or create a standalone app
app = create_app()
```

### Configure Higress WasmPlugin

```yaml
apiVersion: extensions.higress.io/v1alpha1
kind: WasmPlugin
metadata:
  name: gpustack-token-usage
  namespace: higress-system
spec:
  url: http://plugin-server:8080/wasm-plugins/gpustack-token-usage/1.0.0/plugin.wasm
  defaultConfig:
    realIPToHeader: x-gpustack-real-ip
```

## Development

### Prerequisites

- Go 1.24+
- Python 3.10+
- [oras](https://oras.land/) (`brew install oras`) — required for fetching remote plugins

### Build Plugins

```bash
# Install Python dependencies
make dev

# Build all plugins (local + remote, requires oras)
make build

# Build only local Go plugins (no oras required)
make -C extensions build-all

# Build specific plugin
make -C extensions build PLUGIN_NAME=gpustack-token-usage
```

> If `oras` is not installed, `make build` will build local plugins only and print a warning.

### Run Tests

```bash
# Test Go plugins
make test

# Test single plugin
make -C extensions test PLUGIN_NAME=gpustack-token-usage
```

### Check Wheel Contents

```bash
make verify-whl
```

Reports each expected plugin (from `extensions/*/VERSION` and `remote_plugins.yaml`) as ✓ present, ✗ missing, or version mismatch, and checks that `manifest.json` is included.

## Deployment

### Kubernetes (recommended)

Deploy the plugin server as a separate service and reference it from WasmPlugin resources:

```yaml
# Deployment
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpustack-higress-plugins
spec:
  template:
    spec:
      containers:
        - name: plugins
          image: gpustack/higress-plugins:latest
          ports:
            - containerPort: 8080
          livenessProbe:
            httpGet:
              path: /
              port: 8080
          readinessProbe:
            httpGet:
              path: /
              port: 8080
```

## Docker Image

```bash
# Build Docker image
make image

# Build with custom Go proxy
GOPROXY=https://goproxy.cn,direct make image

# Run standalone
docker run -p 8080:8080 gpustack/higress-plugins:latest
```

## Project Structure

```text
gpustack-higress-plugins/
├── extensions/                    # Go plugin source code
│   ├── gpustack-token-usage/
│   │   ├── main.go
│   │   ├── go.mod
│   │   └── VERSION
│   ├── gpustack-set-header-pre-route/
│   ├── remote_plugins.yaml        # Remote OCI plugin config
│   └── Makefile
├── gpustack_higress_plugins/      # Python package
│   ├── __init__.py
│   ├── main.py                    # CLI + FastAPI app factory
│   ├── server.py                  # /wasm-plugins router
│   ├── plugins/                   # Compiled .wasm files (generated)
│   └── manifest.json              # Plugin index (generated)
├── scripts/                       # Build scripts
│   ├── generate_manifest.py
│   ├── generate_metadata.py
│   └── fetch_remote_plugins.py
├── Dockerfile
├── pyproject.toml
└── Makefile
```

## Versioning

- Package version follows Semantic Versioning (MAJOR.MINOR.PATCH)
- Each plugin has its own version in `extensions/<name>/VERSION`
- Package version is set from the git tag at release time (placeholder `0.0.0` in development)
- RC releases (e.g. `0.2.0rc1`) are published to TestPyPI; stable releases go to PyPI

## License

Apache License 2.0
