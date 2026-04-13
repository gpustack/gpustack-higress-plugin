# GPUStack Higress Plugins - Multi-Scenario Deployment Architecture

## Scenario Analysis

### 1. Development Mode

**Environment**: Developer local machine, connecting to remote/local K8s via kubeconfig
**Requirements**:

- Rapid iteration on individual plugins
- Local testing of WASM files
- Direct deployment to development K8s cluster

**Deliverable**: Single `plugin.wasm` file

### 2. All-in-One Mode (Docker Compose/Single Container)

**Environment**: Single-machine Docker deployment, GPUStack + Higress in same container/network
**Requirements**:

- Plugins pre-packaged in image
- Container auto-serves plugins on startup
- No external dependencies

**Deliverable**: Docker image containing all plugins or embedded filesystem

### 3. Helm Deployment Mode (Production K8s)

**Environment**: Production K8s cluster, deployed via Helm
**Requirements**:

- Independent Plugin Server service
- High availability, independently upgradeable
- Standard K8s resource management

**Deliverable**: Helm Chart + Plugin Server image

---

## Build Artifacts Matrix

| Artifact Name           | Format          | Use Case                                   | Build Command            |
| ----------------------- | --------------- | ------------------------------------------ | ------------------------ |
| **Single Plugin WASM**  | `.wasm`         | Development mode, single plugin deployment | `make local-build`       |
| **Plugin Bundle**       | `.tar.gz`       | All-in-One embedded use                    | `make bundle`            |
| **Plugin Server Image** | Docker Image    | K8s deployment                             | `make build-serve-image` |
| **Plugin Manifest**     | `manifest.yaml` | All scenarios (metadata)                   | Auto-generated           |
| **Helm Chart**          | `.tgz`          | Production deployment                      | `make package-chart`     |

---

## Directory Structure Design

```
gpustack-higress-plugins/
├── extensions/                    # Plugin source code
│   ├── gpustack-token-usage/
│   │   ├── main.go
│   │   ├── go.mod
│   │   ├── VERSION
│   │   └── README.md
│   └── gpustack-set-header-pre-route/
│       └── ...
├── build/                         # Build output (.gitignore)
│   ├── plugins/                   # Compiled WASM
│   │   ├── gpustack-token-usage/1.0.0/plugin.wasm
│   │   └── ...
│   └── bundles/                   # Package files
│       └── plugins-{version}.tar.gz
├── deploy/                        # Deployment related
│   ├── helm/                      # Helm Chart
│   │   └── gpustack-plugins/
│   │       ├── Chart.yaml
│   │       ├── values.yaml
│   │       └── templates/
│   ├── k8s/                       # Native K8s YAML
│   │   ├── namespace.yaml
│   │   ├── deployment.yaml
│   │   └── service.yaml
│   └── docker/                    # Docker related
│       ├── Dockerfile.serve       # Plugin server image
│       └── nginx.conf
├── scripts/                       # Build and management scripts
│   ├── build.sh                   # Main build script
│   ├── bundle.sh                  # Packaging script
│   └── plugin_manager.py          # Plugin management tool
├── Makefile                       # Build entry point
├── plugins.yaml                   # Plugin manifest (generated)
└── README.md
```

---

## Workflow Design

### Developer Workflow

```bash
# 1. Develop single plugin
cd extensions/gpustack-token-usage
vim main.go

# 2. Local build test
make local-build PLUGIN_NAME=gpustack-token-usage

# 3. Deploy to dev K8s
kubectl apply -f extensions/gpustack-token-usage/dev-config.yaml

# 4. View logs
kubectl logs -n higress-system deployment/higress-controller -f
```

### Release Process

```bash
# 1. Build all plugins
make build-all

# 2. Create release package
make bundle VERSION=1.0.0

# 3. Build and push plugin server image
make build-serve-image
make build-serve-push

# 4. Package Helm Chart
make package-chart VERSION=1.0.0

# 5. Create GitHub Release (with artifacts)
gh release create v1.0.0 \
  build/bundles/plugins-1.0.0.tar.gz \
  deploy/helm/gpustack-plugins-1.0.0.tgz
```

---

## Scenario Implementation Details

### Scenario 1: Development Mode

**Method A - Direct ConfigMap Mount**

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: gpustack-plugin-wasm
data:
  plugin.wasm: <base64-encoded-wasm>
---
apiVersion: extensions.higress.io/v1alpha1
kind: WasmPlugin
metadata:
  name: gpustack-token-usage
spec:
  url: file:///etc/wasm-plugins/plugin.wasm
```

**Method B - Local HTTP Server**

```bash
# Start simple HTTP server on dev machine
cd build/plugins/gpustack-token-usage/1.0.0
python3 -m http.server 8080

# WasmPlugin config points to local service
url: http://host.docker.internal:8080/plugin.wasm
```

### Scenario 2: All-in-One Mode

**Dockerfile Integration Example**:

```dockerfile
FROM gpustack/gpustack:latest

# Copy plugin package
COPY build/bundles/plugins-1.0.0.tar.gz /tmp/
RUN tar -xzf /tmp/plugins-1.0.0.tar.gz -C /opt/wasm-plugins && \
    rm /tmp/plugins-1.0.0.tar.gz

# Built-in nginx serve plugins
COPY deploy/docker/nginx-internal.conf /etc/nginx/nginx-plugin.conf
COPY deploy/docker/start-with-plugins.sh /usr/local/bin/

ENTRYPOINT ["start-with-plugins.sh"]
```

**Startup Script** (`start-with-plugins.sh`):

```bash
#!/bin/sh
# Start built-in nginx to serve plugins
nginx -c /etc/nginx/nginx-plugin.conf &

# Start GPUStack API Server
# ... existing startup logic
```

### Scenario 3: Helm Deployment Mode

**Chart Structure**:

```
deploy/helm/gpustack-plugins/
├── Chart.yaml
├── values.yaml
└── templates/
    ├── deployment.yaml
    ├── service.yaml
    ├── serviceaccount.yaml
    └── tests/
```

**values.yaml**:

```yaml
# Default values
image:
  repository: gpustack/higress-plugins
  tag: "1.0.0"
  pullPolicy: IfNotPresent

service:
  type: ClusterIP
  port: 8080

resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: 500m
    memory: 512Mi

# Higress cluster configuration
higress:
  namespace: higress-system
  enableIntegration: true # Auto-configure WasmPlugin
```

---

## Version Management Strategy

### Version Number Specification

- Follow SemVer: `MAJOR.MINOR.PATCH`
- All plugins share unified version number
- Increment version number on each release

### Compatibility Matrix

| GPUStack Version | Higress Version | Plugin Version |
| ---------------- | --------------- | -------------- |
| 0.1.x            | 1.3.x           | 1.0.x          |
| 0.2.x            | 1.4.x           | 1.1.x          |

### Changelog

Track changes in `CHANGELOG.md`:

- Added: New plugins or features
- Changed: Feature modifications
- Fixed: Bug fixes
- Deprecated: Features to be removed

---

## CI/CD Integration

### GitHub Actions Workflow

```yaml
name: Build and Release

on:
  push:
    tags:
      - "v*"

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3

      - name: Build All Plugins
        run: |
          make build-all
          make bundle VERSION=${GITHUB_REF#refs/tags/v}

      - name: Build and Push Docker Image
        run: |
          make build-serve-image
          make build-serve-push

      - name: Package Helm Chart
        run: make package-chart

      - name: Create GitHub Release
        run: |
          gh release create ${GITHUB_REF#refs/tags/v} \
            build/bundles/plugins-*.tar.gz \
            deploy/helm/gpustack-plugins-*.tgz
```

---

## Testing Strategy

### Unit Tests

`*_test.go` in each plugin directory:

```bash
make test PLUGIN_NAME=gpustack-token-usage
```

### Integration Tests

Use `kind` to create local K8s cluster for testing:

```bash
make test-integration
```

### E2E Tests

```bash
# Deploy to test environment
helm install gpustack-plugins ./deploy/helm/gpustack-plugins \
  --namespace gpustack-system --create-namespace

# Run test script
./scripts/e2e-test.sh
```
