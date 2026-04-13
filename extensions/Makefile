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

# ORAS binary for fetching OCI artifacts
ORAS ?= oras

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
		if [ -f "$$dir/go.mod" ]; then \
			name=$$(basename "$$dir"); \
			version=$$(cat "$$dir/VERSION" 2>/dev/null || echo "unknown"); \
			echo "  - $$name (version: $$version)"; \
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

# Build a single plugin
build:
	@if [ -z "$(PLUGIN_NAME)" ]; then \
		echo "$(COLOR_YELLOW)Error: PLUGIN_NAME is required$(COLOR_RESET)"; \
		echo "Usage: make build PLUGIN_NAME=gpustack-token-usage"; \
		exit 1; \
	fi
	@if [ ! -d "$(PLUGIN_NAME)" ]; then \
		echo "$(COLOR_YELLOW)Error: Plugin '$(PLUGIN_NAME)' not found$(COLOR_RESET)"; \
		echo "Run 'make list' to see available plugins"; \
		exit 1; \
	fi
	@echo "$(COLOR_GREEN)Building $(PLUGIN_NAME)...$(COLOR_RESET)"
	@cd $(PLUGIN_NAME) && \
		version=$$(cat VERSION) && \
		go mod download && \
		GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(BUILD_FLAGS) -o plugin.wasm . && \
		output_dir="$(OUTPUT_DIR)/$(PLUGIN_NAME)/$$version" && \
		mkdir -p "$$output_dir" && \
		mv plugin.wasm "$$output_dir/" && \
		$(PYTHON) $(SCRIPTS_DIR)/generate_metadata.py "$$output_dir/plugin.wasm" "$(PLUGIN_NAME)" && \
		echo "$(COLOR_GREEN)✓ Built: $$output_dir/plugin.wasm$(COLOR_RESET)"

# Build all local plugins
build-all:
	@echo "$(COLOR_BOLD)Building all local plugins...$(COLOR_RESET)"
	@for dir in */; do \
		if [ -f "$$dir/go.mod" ]; then \
			name=$$(basename "$$dir"); \
			echo "$(COLOR_GREEN)Building $$name...$(COLOR_RESET)"; \
			cd "$$dir" && \
				version=$$(cat VERSION) && \
				go mod download && \
				GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(BUILD_FLAGS) -o plugin.wasm . && \
				output_dir="$(OUTPUT_DIR)/$$name/$$version" && \
				mkdir -p "$$output_dir" && \
				mv plugin.wasm "$$output_dir/" && \
				$(PYTHON) $(SCRIPTS_DIR)/generate_metadata.py "$$output_dir/plugin.wasm" "$$name" && \
				echo "$(COLOR_GREEN)✓ Built: $$output_dir/plugin.wasm$(COLOR_RESET)" && \
				cd ..; \
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

# Test a plugin
test:
	@if [ -z "$(PLUGIN_NAME)" ]; then \
		echo "$(COLOR_YELLOW)Error: PLUGIN_NAME is required$(COLOR_RESET)"; \
		exit 1; \
	fi
	@echo "$(COLOR_GREEN)Testing $(PLUGIN_NAME)...$(COLOR_RESET)"
	@cd $(PLUGIN_NAME) && go test -v ./...

# Test all local plugins
test-all:
	@echo "$(COLOR_BOLD)Testing all local plugins...$(COLOR_RESET)"
	@failed=0; \
	for dir in */; do \
		if [ -f "$$dir/go.mod" ]; then \
			name=$$(basename "$$dir"); \
			echo "$(COLOR_GREEN)Testing $$name...$(COLOR_RESET)"; \
			cd "$$dir" && \
			if go test -v ./...; then \
				cd ..; \
			else \
				cd ..; \
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

.PHONY: list list-remote build build-all fetch-remote fetch-all-remote build-all-with-remote test test-all clean generate-manifest watch help
