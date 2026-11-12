# Makefile for building Higress Wasm plugins
# This Makefile is for building individual Go plugins and fetching remote OCI plugins

# Plugin name (can be overridden)
PLUGIN_NAME ?= gpustack-token-usage

# Go build settings
GOOS := wasip1
GOARCH := wasm
BUILD_FLAGS := -buildmode=c-shared

# Output directory for built WASM files
# Use absolute path to ensure consistency regardless of where Makefile is called from
EXTENSIONS_DIR := $(shell pwd)
OUTPUT_DIR := $(abspath $(EXTENSIONS_DIR)/../gpustack_higress_plugins/plugins)
SCRIPTS_DIR := $(abspath $(EXTENSIONS_DIR)/../scripts)

# Remote plugins configuration file
REMOTE_PLUGINS_CONFIG := remote_plugins.yaml

# Generic wasm-rust build Dockerfile (build context = plugin dir)
RUST_DOCKERFILE := $(abspath $(EXTENSIONS_DIR)/../Dockerfile.rust)

# ---- plugin context (set by .prepare, detected once at parse time) ----
# A plugin's runtime is decided by its manifest file:
#   Cargo.toml -> rust (Docker + Dockerfile.rust, no local toolchain needed)
#   go.mod     -> go   (local Go toolchain, GOOS=wasip1)
PLUGIN_RUNTIME := $(shell \
	if [ -n "$(PLUGIN_NAME)" ] && [ -f "$(PLUGIN_NAME)/Cargo.toml" ]; then echo rust; \
	elif [ -n "$(PLUGIN_NAME)" ] && [ -f "$(PLUGIN_NAME)/go.mod" ]; then echo go; \
	fi)
PLUGIN_VERSION := $(strip $(shell cat $(PLUGIN_NAME)/VERSION 2>/dev/null))
PLUGIN_OUTPUT_DIR = $(OUTPUT_DIR)/$(PLUGIN_NAME)/$(PLUGIN_VERSION)

# ORAS binary for fetching OCI artifacts
ORAS ?= oras

# Docker registry for OCI plugin images
DOCKER_REGISTRY ?= docker.io

# Python binary for scripts (use parent venv if available, otherwise system python)
PYTHON ?= $(if $(wildcard $(EXTENSIONS_DIR)/../.venv/bin/python),$(EXTENSIONS_DIR)/../.venv/bin/python,python3)

# Color output
COLOR_RESET := \033[0m
COLOR_BOLD := \033[1m
COLOR_GREEN := \033[32m
COLOR_YELLOW := \033[33m
COLOR_BLUE := \033[34m

.DEFAULT:
	@echo "Available targets:"
	@echo "  make build                        - Build a single plugin (PLUGIN_NAME=name)"
	@echo "  make build-all                    - Build all local plugins"
	@echo "  make fetch-remote PLUGIN=...      - Fetch a remote OCI plugin"
	@echo "  make fetch-all-remote             - Fetch all configured remote plugins"
	@echo "  make build-all-with-remote        - Build local + fetch remote plugins"
	@echo "  make clean                        - Clean build outputs"
	@echo "  make list                         - List all available plugins"
	@echo "  make list-remote                  - List configured remote plugins"
	@echo "  make test PLUGIN_NAME=name        - Test a single plugin"
	@echo "  make test-all                     - Test all local plugins"
	@echo "  make build-oci-image              - Build OCI plugin image (PLUGIN_NAME=name)"
	@echo "  make push-oci-image               - Push OCI plugin image (PLUGIN_NAME=name)"
	@echo ""
	@echo "Remote Plugin Configuration (remote_plugins.yaml):"
	@echo "  default_registry: registry.example.com/plugins"
	@echo "  remote_plugins:"
	@echo "    - source: plugin:tag            # Uses default_registry"
	@echo "      name: local-name"
	@echo "      version: 1.0.0"
	@echo "      digest: sha256:...            # Optional, for version pinning"
	@echo "    - source: oci://other/registry/plugin:tag  # Override registry"
	@echo "      name: other-plugin"
	@echo "      version: 1.0.0"
	@echo ""
	@echo "Examples:"
	@echo "  make build PLUGIN_NAME=gpustack-token-usage"
	@echo "  make build-all"
	@echo "  make fetch-all-remote"

# List all local plugins
list:
	@echo "$(COLOR_BOLD)Local plugins:$(COLOR_RESET)"
	@for dir in */; do \
		if [ -f "$$dir/go.mod" ] || [ -f "$$dir/Cargo.toml" ]; then \
			name=$$(basename "$$dir"); \
			version=$$(cat "$$dir/VERSION" 2>/dev/null || echo "unknown"); \
			if [ -f "$$dir/Cargo.toml" ]; then \
				runtime="rust"; \
			else \
				runtime="go"; \
			fi; \
			if [ -f "$$dir/.nopackage" ]; then \
				echo "  - $$name (version: $$version, runtime: $$runtime) $(COLOR_YELLOW)[not packaged]$(COLOR_RESET)"; \
			else \
				echo "  - $$name (version: $$version, runtime: $$runtime)"; \
			fi; \
		fi; \
	done

# List configured remote plugins
list-remote:
	@echo "$(COLOR_BOLD)Remote plugins:$(COLOR_RESET)"
	@if [ -z "$(REMOTE_PLUGINS)" ]; then \
		echo "  $(COLOR_YELLOW)No remote plugins configured$(COLOR_RESET)"; \
		echo "  Set REMOTE_PLUGINS in Makefile or via environment variable"; \
	else \
		for plugin in $(REMOTE_PLUGINS); do \
			oci_url=$$(echo $$plugin | cut -d'|' -f1); \
			local_name=$$(echo $$plugin | cut -d'|' -f2); \
			version=$$(echo $$plugin | cut -d'|' -f3); \
			echo "  - $$local_name (version: $$version)"; \
			echo "    Source: $$oci_url"; \
		done; \
	fi

# Validate PLUGIN_NAME and set up the plugin context. Runs before build/test.
# Hidden target (leading dot): always executes, never shows as a file.
.prepare:
	@if [ -z "$(PLUGIN_NAME)" ]; then \
		echo "$(COLOR_YELLOW)Error: PLUGIN_NAME is required$(COLOR_RESET)"; \
		echo "Usage: make $(MAKECMDGOALS) PLUGIN_NAME=gpustack-token-usage"; \
		exit 1; \
	fi
	@if [ ! -d "$(PLUGIN_NAME)" ]; then \
		echo "$(COLOR_YELLOW)Error: Plugin '$(PLUGIN_NAME)' not found$(COLOR_RESET)"; \
		echo "Run 'make list' to see available plugins"; \
		exit 1; \
	fi
	@if [ -z "$(PLUGIN_RUNTIME)" ]; then \
		echo "$(COLOR_YELLOW)Error: '$(PLUGIN_NAME)' has neither go.mod nor Cargo.toml$(COLOR_RESET)"; \
		exit 1; \
	fi
	@echo "$(COLOR_BLUE)Context: plugin=$(PLUGIN_NAME) runtime=$(PLUGIN_RUNTIME) version=$(PLUGIN_VERSION) output=$(PLUGIN_OUTPUT_DIR)$(COLOR_RESET)"

.PHONY: .prepare

# ---- dispatch: pure routing, all logic lives in the *-go / *-rust targets ----
ifeq ($(PLUGIN_RUNTIME),rust)
build: .prepare build-rust
test: .prepare test-rust
else
build: .prepare build-go
test: .prepare test-go
endif

# Build a wasm-rust plugin in Docker (see Dockerfile.rust at repo root).
# Set CARGO_DIRECT=1 to run cargo directly (used inside container builds
# where no Docker daemon is available, e.g. the rust-builder image stage).
build-rust:
	@if [ "$(CARGO_DIRECT)" = "1" ]; then \
		cd $(PLUGIN_NAME) && \
			cargo build --target wasm32-wasip1 --release && \
			mkdir -p $(PLUGIN_OUTPUT_DIR) && \
			cp target/wasm32-wasip1/release/*.wasm $(PLUGIN_OUTPUT_DIR)/plugin.wasm && \
			cargo clean; \
	else \
		cd $(PLUGIN_NAME) && \
			rm -rf dist && \
			DOCKER_BUILDKIT=1 docker build \
				-f $(RUST_DOCKERFILE) \
				--platform linux/amd64 \
				--build-arg PROXY=$(PROXY) \
				--output dist . && \
			mkdir -p $(PLUGIN_OUTPUT_DIR) && \
			mv dist/plugin.wasm $(PLUGIN_OUTPUT_DIR)/ && \
			rm -rf dist; \
	fi && \
		$(PYTHON) $(SCRIPTS_DIR)/generate_metadata.py $(PLUGIN_OUTPUT_DIR)/plugin.wasm $(PLUGIN_NAME) && \
		echo "$(COLOR_GREEN)✓ Built: $(PLUGIN_OUTPUT_DIR)/plugin.wasm$(COLOR_RESET)"

# Build a wasm-go plugin with the local Go toolchain
build-go:
	@cd $(PLUGIN_NAME) && \
		go mod download && \
		GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(BUILD_FLAGS) -o plugin.wasm . && \
		mkdir -p $(PLUGIN_OUTPUT_DIR) && \
		mv plugin.wasm $(PLUGIN_OUTPUT_DIR)/ && \
		$(PYTHON) $(SCRIPTS_DIR)/generate_metadata.py $(PLUGIN_OUTPUT_DIR)/plugin.wasm $(PLUGIN_NAME) && \
		echo "$(COLOR_GREEN)✓ Built: $(PLUGIN_OUTPUT_DIR)/plugin.wasm$(COLOR_RESET)"

# Test a wasm-rust plugin in Docker (Dockerfile.rust test stage)
test-rust:
	@cd $(PLUGIN_NAME) && \
		DOCKER_BUILDKIT=1 docker build \
			-f $(RUST_DOCKERFILE) \
			--platform linux/amd64 \
			--build-arg PROXY=$(PROXY) \
			--target test .

# Test a wasm-go plugin with the local Go toolchain
test-go:
	@cd $(PLUGIN_NAME) && go test -v ./...

# Build all local plugins
#
# A plugin directory containing a .nopackage marker is skipped: its source is
# kept and tested, but its wasm is not built and therefore never reaches the
# wheel. The marker is a file rather than a list in this Makefile because
# verify_whl.py has to agree about it, and the go-builder stage in the
# Dockerfile has no Python to parse a shared config with (that is the whole
# point of the PYTHON=true trick) -- a plain file is the only representation
# both sides can read without duplicating the fact.
#
# Skipping also **deletes any existing output** for that plugin. Leaving it
# would be worse than never building it: the wheel force-includes whatever sits
# in OUTPUT_DIR, and the root `make build` neither cleans nor runs
# verify_whl.py, so a checkout that built the plugin before the marker existed
# would keep shipping that stale wasm indefinitely.
#
# `make build PLUGIN_NAME=<name>` ignores the marker on purpose: naming a plugin
# explicitly is the escape hatch.
# Build all local plugins. Set RUNTIME=go or RUNTIME=rust to build only
# plugins of that runtime (used by the split go-builder/rust-builder stages
# in the root Dockerfile, where no Docker daemon is available).
build-all:
	@echo "$(COLOR_BOLD)Building all local plugins...$(COLOR_RESET)"
	@for dir in */; do \
		if [ -f "$$dir/go.mod" ] || [ -f "$$dir/Cargo.toml" ]; then \
			name=$$(basename "$$dir"); \
			if [ -f "$$dir/Cargo.toml" ]; then \
				runtime=rust; \
			else \
				runtime=go; \
			fi; \
			if [ -n "$(RUNTIME)" ] && [ "$$runtime" != "$(RUNTIME)" ]; then \
				echo "$(COLOR_YELLOW)Skipping $$name (runtime $$runtime != $(RUNTIME))$(COLOR_RESET)"; \
				continue; \
			fi; \
			if [ -f "$$dir/.nopackage" ]; then \
				echo "$(COLOR_YELLOW)Skipping $$name (.nopackage)$(COLOR_RESET)"; \
				if [ -d "$(OUTPUT_DIR)/$$name" ]; then \
					echo "$(COLOR_YELLOW)  removing stale output $(OUTPUT_DIR)/$$name$(COLOR_RESET)"; \
					rm -rf "$(OUTPUT_DIR)/$$name"; \
				fi; \
				continue; \
			fi; \
			$(MAKE) build PLUGIN_NAME=$$name || exit 1; \
		fi; \
	done
	@echo "$(COLOR_GREEN)All local plugins built successfully!$(COLOR_RESET)"
	@# Note: manifest generation skipped in Go-only build, will be done in Python stage

# Fetch a single remote OCI plugin
fetch-remote:
	@echo "$(COLOR_BLUE)Fetching remote plugin...$(COLOR_RESET)"
	@if [ -z "$(PLUGIN)" ]; then \
		echo "$(COLOR_YELLOW)Error: PLUGIN parameter is required$(COLOR_RESET)"; \
		echo "Usage: make fetch-remote PLUGIN='oci://registry/name:tag|local-name|version'"; \
		exit 1; \
	fi
	@oci_url=$$(echo $(PLUGIN) | cut -d'|' -f1); \
	local_name=$$(echo $(PLUGIN) | cut -d'|' -f2); \
	version=$$(echo $(PLUGIN) | cut -d'|' -f3); \
	$(PYTHON) $(SCRIPTS_DIR)/fetch_remote_plugins.py \
		--source "$$oci_url" \
		--name "$$local_name" \
		--version "$$version" \
		--output-dir "$(OUTPUT_DIR)" \
		--oras "$(ORAS)"

# Fetch all configured remote plugins
fetch-all-remote:
	@echo "$(COLOR_BOLD)Fetching all remote plugins...$(COLOR_RESET)"
	@if [ ! -f "$(REMOTE_PLUGINS_CONFIG)" ]; then \
		echo "$(COLOR_YELLOW)No remote plugins config found: $(REMOTE_PLUGINS_CONFIG)$(COLOR_RESET)"; \
		echo "  Create $(REMOTE_PLUGINS_CONFIG) to configure remote plugins"; \
		exit 1; \
	fi
	@$(PYTHON) ../scripts/fetch_remote_plugins.py --config $(REMOTE_PLUGINS_CONFIG) --oras $(ORAS)

# Build local plugins and fetch remote plugins
build-all-with-remote: build-all
	@if [ ! -f "$(REMOTE_PLUGINS_CONFIG)" ]; then \
		echo "$(COLOR_YELLOW)No remote plugins config found, skipping$(COLOR_RESET)"; \
	elif ! $(ORAS) version > /dev/null 2>&1; then \
		echo "$(COLOR_YELLOW)oras not found, skipping remote plugins (install with: brew install oras)$(COLOR_RESET)"; \
	else \
		$(MAKE) fetch-all-remote; \
	fi

# Test all local plugins
test-all:
	@echo "$(COLOR_BOLD)Testing all local plugins...$(COLOR_RESET)"
	@failed=0; \
	for dir in */; do \
		if [ -f "$$dir/go.mod" ] || [ -f "$$dir/Cargo.toml" ]; then \
			name=$$(basename "$$dir"); \
			if $(MAKE) test PLUGIN_NAME=$$name; then \
				:; \
			else \
				failed=1; \
			fi; \
		fi; \
	done; \
	if [ $$failed -eq 1 ]; then \
		echo "$(COLOR_YELLOW)✗ Some tests failed$(COLOR_RESET)"; \
		exit 1; \
	fi
	@echo "$(COLOR_GREEN)All tests passed!$(COLOR_RESET)"

# Clean build outputs
clean:
	@echo "$(COLOR_YELLOW)Cleaning build outputs...$(COLOR_RESET)"
	@rm -rf ../gpustack_higress_plugins/plugins
	@rm -rf ../gpustack_higress_plugins/manifest.json
	@rm -rf ./gpustack_higress_plugins  # Clean up incorrectly placed artifacts
	@rm -f */plugin.wasm */main.wasm
	@echo "$(COLOR_GREEN)Clean complete$(COLOR_RESET)"

# Generate manifest.json
generate-manifest:
	@echo "$(COLOR_GREEN)Generating manifest.json...$(COLOR_RESET)"
	@cd .. && $(PYTHON) scripts/generate_manifest.py

# Build OCI plugin image
# Depends on build to ensure the .wasm is up to date before packaging.
# Build context is the repo root so Dockerfile.oci-plugin can reach gpustack_higress_plugins/plugins/.
build-oci-image: build
	@version=$$(cat $(PLUGIN_NAME)/VERSION); \
	image=$(DOCKER_REGISTRY)/$(PLUGIN_NAME):$$version; \
	echo "$(COLOR_GREEN)Building OCI image $$image...$(COLOR_RESET)"; \
	docker build \
		--build-arg PLUGIN_NAME=$(PLUGIN_NAME) \
		--build-arg PLUGIN_VERSION=$$version \
		-f $(EXTENSIONS_DIR)/../Dockerfile.oci-plugin \
		-t $$image \
		$(EXTENSIONS_DIR)/..; \
	echo "$(COLOR_GREEN)✓ Built: $$image$(COLOR_RESET)"

# Push OCI plugin image (image must be built first)
push-oci-image:
	@if [ -z "$(PLUGIN_NAME)" ]; then \
		echo "$(COLOR_YELLOW)Error: PLUGIN_NAME is required$(COLOR_RESET)"; \
		exit 1; \
	fi
	@version=$$(cat $(PLUGIN_NAME)/VERSION); \
	image=$(DOCKER_REGISTRY)/$(PLUGIN_NAME):$$version; \
	echo "$(COLOR_BLUE)Pushing OCI image $$image...$(COLOR_RESET)"; \
	docker push $$image; \
	echo "$(COLOR_GREEN)✓ Pushed: $$image$(COLOR_RESET)"

# Watch for changes and rebuild (development)
watch:
	@if [ -z "$(PLUGIN_NAME)" ]; then \
		echo "$(COLOR_YELLOW)Error: PLUGIN_NAME is required$(COLOR_RESET)"; \
		exit 1; \
	fi
	@echo "$(COLOR_GREEN)Watching $(PLUGIN_NAME) for changes...$(COLOR_RESET)"
	@which fswatch > /dev/null 2>&1 || { \
		echo "$(COLOR_YELLOW)Error: fswatch not installed$(COLOR_RESET)"; \
		echo "Install with: brew install fswatch"; \
		exit 1; \
	}
	@fswatch -o $(PLUGIN_NAME)/*.go | xargs -n1 -I{} make build PLUGIN_NAME=$(PLUGIN_NAME)

# Help target
help:
	@echo "$(COLOR_BOLD)GPUStack Higress Plugins - Build Help$(COLOR_RESET)"
	@echo ""
	@echo "$(COLOR_BOLD)Local Plugins:$(COLOR_RESET)"
	@echo "  build              - Build a single plugin (requires PLUGIN_NAME)"
	@echo "  build-all          - Build all local plugins"
	@echo ""
	@echo "$(COLOR_BOLD)Remote OCI Plugins:$(COLOR_RESET)"
	@echo "  fetch-remote       - Fetch a remote OCI plugin (requires PLUGIN)"
	@echo "  fetch-all-remote  - Fetch all configured remote plugins"
	@echo "  build-all-with-remote - Build local + fetch remote"
	@echo "  list-remote        - List configured remote plugins"
	@echo ""
	@echo "$(COLOR_BOLD)Utility:$(COLOR_RESET)"
	@echo "  clean              - Clean build outputs"
	@echo "  list               - List all available plugins"
	@echo "  test               - Test a single plugin (requires PLUGIN_NAME)"
	@echo "  test-all           - Test all local plugins"
	@echo "  generate-manifest  - Generate manifest.json"
	@echo "  watch              - Watch for changes and rebuild (requires PLUGIN_NAME)"
	@echo ""
	@echo "$(COLOR_BOLD)Remote Plugin Format:$(COLOR_RESET)"
	@echo "  PLUGIN='oci://registry/name:tag|local-name|version'"
	@echo ""
	@echo "$(COLOR_BOLD)Examples:$(COLOR_RESET)"
	@echo "  make build PLUGIN_NAME=gpustack-token-usage"
	@echo "  make build-all"
	@echo "  make fetch-remote PLUGIN='oci://ghcr.io/higress-extensions/ai-proxy:1.2.3|ai-proxy|1.2.3'"
	@echo "  make build-all-with-remote"

.PHONY: list list-remote build build-go build-rust build-all fetch-remote fetch-all-remote build-all-with-remote test test-go test-rust test-all clean generate-manifest watch build-oci-image push-oci-image help
