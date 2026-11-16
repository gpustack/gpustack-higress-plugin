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

Docker images are **not** built by CI on push or release. They are built
manually via the `image` workflow (`.github/workflows/image.yaml`), which:

1. Takes a release tag (e.g. `v0.2.0`) as input — only version tags are allowed.
2. Downloads the `.whl` from that tag's GitHub Release.
3. Builds the runtime image with `Dockerfile.whl`, which simply installs the
   downloaded wheel (WASM plugins are arch-independent, so the pure-Python whl
   is the single build artifact).

Trigger it from GitHub: Actions → image → Run workflow → enter the tag.

Locally, you can reproduce the image build:

```bash
# 1. Clean dist/ and download the released wheel
rm -rf dist/ && gh release download v0.2.0 --pattern '*.whl' --dir dist/

# 2. Build the image
docker build -f Dockerfile.whl -t gpustack/higress-plugins:v0.2.0 .
```

The source `Dockerfile` (runtime stage) is still available for local
source-based builds:

```bash
docker build -t gpustack-higress-plugins .
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

Images are released manually from a tagged release wheel via the `image`
workflow (see "Build Docker Image" above):

```bash
# From GitHub: Actions → image → Run workflow → enter the tag
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
