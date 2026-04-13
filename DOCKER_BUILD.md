# Docker Build Guide

## Overview

The Dockerfile supports two build modes:

1. **whl-builder** - Build Python wheel package (for PyPI release)
2. **runtime** - Build final Docker image (with complete application)

This ensures Docker images and PyPI packages use **identical** build logic.

## Usage

### Build Wheel Package (reuse Docker build logic)

```bash
# Build wheel using Docker, ensuring consistency with Docker image
make build-docker

# Or manually
docker build --target=whl-output --output=type=local,dest=./dist .
```

Generated wheel files are located in `dist/`, containing:

- All Python code
- Built Go plugins (.wasm files)
- manifest.json and metadata.txt

**Technical notes**:
- Using `scratch` base image + `docker build --output` follows Docker best practices
- `whl-output` stage only contains `/dist` directory for minimal size
- Extract files directly from build stage, no need to start temporary container

### Build Docker Image

```bash
# Build complete image (with runtime)
make image

# Or
DOCKER_HOST=unix:///var/run/docker.sock docker build -t gpustack-higress-plugins .
```

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    Docker Build Flow                        │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  ┌──────────────┐     ┌──────────────┐                    │
│  │ go-builder   │     │ whl-builder  │                    │
│  │               │     │               │                    │
│  │ - Build Go    │     │ - Copy source │                    │
│  │   plugins     │────→│ - Build wheel │────→ /dist/*.whl  │
│  │               │     │ - Fetch remote│         │          │
│  └───────────────┘     │   plugins     │         │          │
│                       └───────────────┘         ↓          │
│  ┌──────────────┐                ┌──────────────┐          │
│  │   runtime    │                │ whl-output   │          │
│  │               │                │ (scratch)    │          │
│  │ - Install     │←─────── whl    │ - Copy /dist │────→     │
│  │   wheel       │         from   │ - Extract    │  ./dist/ │
│  │ - Run server  │         builder └──────────────┘         │
│  └──────────────┘                                        │
└─────────────────────────────────────────────────────────────┘
```

**New `whl-output` stage advantages**:

- **Minimal image size**: Using `scratch` base image, only contains `/dist` directory
- **Direct extraction**: Extract files via `docker build --output`, no container needed
- **Docker best practice**: Follows official recommended artifact extraction method

## Release Flow

### PyPI Release

```bash
# 1. Build wheel using Docker (consistent with image)
make build-docker

# 2. Check wheel contents
ls -lh dist/

# 3. Upload to PyPI
twine upload dist/*.whl
```

### Docker Image Release

```bash
# 1. Build image
make image

# 2. Push to registry
make push
```

## Environment Variables

- `DOCKER_HOST` - Docker daemon address (default: auto-detect)
  - macOS/Linux: `unix:///var/run/docker.sock`
  - Example: `DOCKER_HOST=unix:///var/run/docker.sock make build-docker`

## Verification

```bash
# Check wheel package contents
python3 -c "
import zipfile
z = zipfile.ZipFile('dist/gpustack_higrestack_higress_plugins-1.0.0-py3-none-any.whl')
print('Files:', len(z.namelist()))
for name in sorted(z.namelist()):
    print(f'  {name}')
"
```
