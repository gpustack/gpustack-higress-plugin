# Makefile for GPUStack Higress Plugins

# Package settings
VENV := .venv
PACKAGE_NAME := gpustack_higress_plugins
DOCKER_IMAGE := gpustack/higress-plugins
DOCKER_REGISTRY := docker.io
DOCKER_TAG := $(shell grep "^version" pyproject.toml | head -1 | sed 's/version = "\([^"]*\)".*/\1/')

# Build directories
DIST_DIR := dist
BUILD_DIR := build

# Color output
COLOR_RESET := \033[0m
COLOR_BOLD := \033[1m
COLOR_GREEN := \033[32m
COLOR_YELLOW := \033[33m
COLOR_BLUE := \033[34m

.DEFAULT:
	@echo "$(COLOR_BOLD)GPUStack Higress Plugins$(COLOR_RESET)"
	@echo ""
	@echo "Available targets:"
	@echo "  make build              - Build plugins and Python package"
	@echo "  make build-docker       - Build wheel using Docker"
	@echo "  make venv               - Create virtual environment"
	@echo "  make install            - Install package in editable mode"
	@echo "  make dev                - Setup development environment"
	@echo "  make image              - Build Docker image"
	@echo "  make push               - Push Docker image to registry"
	@echo "  make clean              - Clean build artifacts"
	@echo "  make test               - Test Go plugins"
	@echo "  make lint               - Run linting"
	@echo "  make format             - Format code"
	@echo "  make check-dirty        - Check for uncommitted changes"

# Create virtual environment
venv:
	@echo "$(COLOR_BLUE)Creating virtual environment at $(VENV)...$(COLOR_RESET)"
	@python3 -m venv $(VENV)
	@echo "$(COLOR_GREEN)✓ Virtual environment created$(COLOR_RESET)"
	@echo "  Run 'source $(VENV)/bin/activate' to activate"

# Install locally in editable mode
install: venv
	@echo "$(COLOR_BLUE)Installing $(PACKAGE_NAME) in editable mode...$(COLOR_RESET)"
	@$(VENV)/bin/pip install --no-cache-dir uv
	@$(VENV)/bin/uv sync
	@echo "$(COLOR_GREEN)✓ Package installed$(COLOR_RESET)"
	@echo "  Activate with: source $(VENV)/bin/activate"

# Development setup
dev: venv
	@echo "$(COLOR_BLUE)Setting up development environment...$(COLOR_RESET)"
	@$(VENV)/bin/pip install --no-cache-dir uv
	@$(VENV)/bin/uv sync --dev --no-install-project
	@$(VENV)/bin/pre-commit install
	@echo "$(COLOR_GREEN)✓ Development environment ready$(COLOR_RESET)"
	@echo "  Activate with: source $(VENV)/bin/activate"

# Build everything (plugins + Python package)
build:
	@echo "$(COLOR_BLUE)Building Go plugins and fetching remote plugins...$(COLOR_RESET)"
	@$(MAKE) -C extensions build-all-with-remote
	@echo "$(COLOR_BLUE)Generating manifest...$(COLOR_RESET)"
	@if [ -d ".venv" ]; then \
		.venv/bin/python scripts/generate_manifest.py; \
	else \
		python3 scripts/generate_manifest.py; \
	fi
	@echo "$(COLOR_BLUE)Building Python package...$(COLOR_RESET)"
	@if [ -d ".venv" ]; then \
		.venv/bin/pip install --no-cache-dir uv; \
		.venv/bin/uv build --wheel; \
	else \
		pip install --no-cache-dir uv; \
		uv build --wheel; \
	fi
	@echo "$(COLOR_GREEN)✓ Build complete$(COLOR_RESET)"
	@ls -lh $(DIST_DIR)/

# Build wheel using Docker
build-docker:
	@echo "$(COLOR_BLUE)Building wheel using Docker...$(COLOR_RESET)"
	@rm -rf $(DIST_DIR)
	@docker build \
		$(if $(GOPROXY),--build-arg GOPROXY=$(GOPROXY)) \
		--target=whl-output --output=type=local,dest=$(DIST_DIR) .
	@echo "$(COLOR_GREEN)✓ Wheel built using Docker$(COLOR_RESET)"
	@ls -lh $(DIST_DIR)/*.whl

# Build Docker image
image:
	@echo "$(COLOR_BLUE)Building Docker image $(DOCKER_REGISTRY)/$(DOCKER_IMAGE):$(DOCKER_TAG)...$(COLOR_RESET)"
	@docker build \
		$(if $(GOPROXY),--build-arg GOPROXY=$(GOPROXY)) \
		-t $(DOCKER_REGISTRY)/$(DOCKER_IMAGE):$(DOCKER_TAG) .
	@docker tag $(DOCKER_REGISTRY)/$(DOCKER_IMAGE):$(DOCKER_TAG) $(DOCKER_REGISTRY)/$(DOCKER_IMAGE):latest
	@echo "$(COLOR_GREEN)✓ Docker image built$(COLOR_RESET)"
	@docker images $(DOCKER_IMAGE)

# Push Docker image
push:
	@echo "$(COLOR_BLUE)Pushing Docker image...$(COLOR_RESET)"
	@docker push $(DOCKER_REGISTRY)/$(DOCKER_IMAGE):$(DOCKER_TAG)
	@docker push $(DOCKER_REGISTRY)/$(DOCKER_IMAGE):latest
	@echo "$(COLOR_GREEN)✓ Docker image pushed$(COLOR_RESET)"

# Run Go plugin tests
test:
	@echo "$(COLOR_BLUE)Testing Go plugins...$(COLOR_RESET)"
	@$(MAKE) -C extensions test-all

# Run linting
lint: venv
	@echo "$(COLOR_BLUE)Running linters...$(COLOR_RESET)"
	@$(VENV)/bin/pip install --no-cache-dir uv
	@$(VENV)/bin/uv sync --dev --no-install-project
	@$(VENV)/bin/ruff check gpustack_higress_plugins/ scripts/

# Format code
format: venv
	@echo "$(COLOR_BLUE)Formatting code...$(COLOR_RESET)"
	@$(VENV)/bin/pip install --no-cache-dir uv
	@$(VENV)/bin/uv sync --dev --no-install-project
	@$(VENV)/bin/ruff check --fix gpustack_higress_plugins/ scripts/
	@$(VENV)/bin/ruff format gpustack_higress_plugins/ scripts/

# Clean build artifacts
clean:
	@echo "$(COLOR_YELLOW)Cleaning build artifacts...$(COLOR_RESET)"
	@rm -rf $(BUILD_DIR) $(DIST_DIR) *.egg-info
	@rm -rf gpustack_higress_plugins/plugins
	@find . -type d -name __pycache__ -exec rm -rf {} + 2>/dev/null || true
	@find . -type f -name "*.pyc" -delete 2>/dev/null || true
	@find . -type d -name ".ruff_cache" -exec rm -rf {} + 2>/dev/null || true
	@find . -type d -name ".pytest_cache" -exec rm -rf {} + 2>/dev/null || true
	@find . -type d -name ".mypy_cache" -exec rm -rf {} + 2>/dev/null || true
	@$(MAKE) -C extensions clean
	@echo "$(COLOR_GREEN)✓ Clean complete$(COLOR_RESET)"

# Clean venv
clean-venv:
	@echo "$(COLOR_YELLOW)Removing virtual environment...$(COLOR_RESET)"
	@rm -rf $(VENV)
	@echo "$(COLOR_GREEN)✓ Virtual environment removed$(COLOR_RESET)"

# Verify whl contains all local plugins (use --local-only to skip remote plugins)
# Dockerfile always verifies all plugins (local + remote) directly via verify_whl.py
verify-whl:
	@echo "$(COLOR_BLUE)Verifying wheel contents (local plugins only)...$(COLOR_RESET)"
	@if [ -d ".venv" ]; then \
		.venv/bin/python scripts/verify_whl.py --local-only; \
	else \
		python3 scripts/verify_whl.py --local-only; \
	fi
	@echo "$(COLOR_GREEN)✓ Wheel verified$(COLOR_RESET)"

# Check for uncommitted changes after lint
check-dirty:
	@echo "$(COLOR_BLUE)Checking for uncommitted changes...$(COLOR_RESET)"
	@if git diff --quiet; then \
		echo "$(COLOR_GREEN)✓ No uncommitted changes$(COLOR_RESET)"; \
	else \
		echo "$(COLOR_YELLOW)✗ Lint made changes! Please run 'make lint' locally and commit.$(COLOR_RESET)"; \
		git diff --stat; \
		exit 1; \
	fi

.PHONY: venv install dev build build-docker image push test lint format clean clean-venv check-dirty verify-whl
