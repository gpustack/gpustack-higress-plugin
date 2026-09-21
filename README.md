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

- **gpustack-ip-acl** - Per-consumer source-IP blacklist/whitelist, mirroring the GPUStack Enterprise API key IP ACL semantics at the gateway. Rules are keyed by the Higress consumer identity (`x-mse-consumer`, injected by `gpustack-ext-auth`); `deniedCidrs` always take precedence over `allowedCidrs`, consumer keys support `*` wildcards, and invalid client IPs fail closed. Supports per-route overrides via Higress `matchRules`. Deploy at `AUTHN/350`, immediately after ext-auth.

- **gpustack-lb** - The single binary of the LB plugin framework (`mode: context` / `mode: finisher`), replacing model-mapper: publishes candidate instances, then picks one by weighted-sum ranking and steers the route via `x-higress-target-cluster`.

- **gpustack-lb-session-affinity** - LB capability plugin: keeps one session's requests on the same instance.

- **gpustack-lb-least-load** - LB capability plugin: sends each request to the least-loaded instance.

See each plugin's `README.md` and `example.yaml` under `extensions/` for full configuration and deployment details.

## Filter-Chain Ordering

Plugins are positioned by `phase` (bucket order: AUTHN → … → UNSPECIFIED; buckets beat raw priority numbers) and `priority` (descending within a phase). The intended chain — all rejection points ahead of any scheduling-state work:

```text
AUTHN       900 model-router → 810 transformer (strips spoofed identity headers)
            → 360 gpustack-ext-auth (injects trusted x-mse-consumer; 401)
            → 350 gpustack-ip-acl (403)
            → 340-325 LB band (context → session-affinity / prefix / least-load → finisher)
UNSPECIFIED 600 gpustack-rate-limit (429) → 400 gpustack-token-usage
            → 100 ai-proxy → router
```

When changing any plugin's position, re-check its ordering constraints (documented in each plugin's README) and verify the live filter chain via Envoy `config_dump` after rollout.

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
│   ├── gpustack-ip-acl/             # Per-consumer IP blacklist/whitelist
│   ├── gpustack-lb/                 # LB framework (context/finisher roles)
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
