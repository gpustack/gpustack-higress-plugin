# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Higress Proxy-Wasm plugins for GPUStack, targeting AI API traffic processing and observability. Each plugin under `extensions/` is a standalone Go module compiled to WebAssembly (`plugin.wasm`) and deployed to Higress-compatible gateways.

## Build Commands

```bash
# Build a specific plugin (outputs extensions/<plugin>/plugin.wasm)
make build PLUGIN_NAME=gpustack-token-usage
make build PLUGIN_NAME=gpustack-set-header-pre-route

# Build and push Docker image
make build-push PLUGIN_NAME=<plugin-name>

# Local build (no Docker, outputs main.wasm)
make local-build PLUGIN_NAME=<plugin-name>
```

Build uses Docker with a specialized WASM Go builder image (`higress-registry.cn-hangzhou.cr.aliyuncs.com/plugins/wasm-go-builder`). The WASM compilation target is `GOOS=wasip1 GOARCH=wasm` with `-buildmode=c-shared`.

## Testing

```bash
# Run tests for a specific plugin
cd extensions/<plugin-name> && go test ./...
```

## Architecture

- **Plugin model**: Each plugin in `extensions/` is an independent Go module with its own `go.mod`, `VERSION`, and `README.md`.
- **SDK dependencies**: All plugins use `github.com/higress-group/proxy-wasm-go-sdk` and `github.com/higress-group/wasm-go` (Higress's Go SDK for Wasm plugins). The `wasm-go/pkg/wrapper` package provides the plugin framework.
- **JSON handling**: `tidwall/gjson` and `tidwall/sjson` for JSON parsing/manipulation in Wasm context.
- **Plugin lifecycle**: Plugins implement `wrapper.ProcessRequestHeaders`, `wrapper.ProcessRequestBody`, `wrapper.ProcessResponseHeaders`, and `wrapper.ProcessResponseBody` callbacks.
- **Streaming**: SSE (Server-Sent Events) responses are handled by buffering and parsing chunked data across multiple `ProcessResponseBody` calls.

## CI/CD

Tag pushes trigger `.github/workflows/push.yaml` which builds all plugins, pushes Docker images, and creates GitHub releases with `.tar.gz` archives of each `plugin.wasm`.

## Available Plugins

- **gpustack-token-usage**: Collects token usage stats from AI API streaming responses (TTFT, avg token latency, tokens/sec). Injects real client IP. Configurable path filtering.
- **gpustack-set-header-pre-route**: Injects route name and cluster/model name into request headers before routing. Path-based filtering via suffixes and prefixes.
