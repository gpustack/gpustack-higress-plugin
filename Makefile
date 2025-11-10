PLUGIN_NAME ?= gpustack-token-usage
PLUGIN_VERSION := $(shell cat extensions/${PLUGIN_NAME}/VERSION)
BUILDER_REGISTRY ?= higress-registry.cn-hangzhou.cr.aliyuncs.com/plugins/
GITHUB_REPOSITORY ?= gpustack/gpustack-token-usage
DOCKER_ORG ?= $(shell echo $(GITHUB_REPOSITORY) | cut -d'/' -f1)
REGISTRY ?= docker.io/$(DOCKER_ORG)/higress-plugin-
GO_VERSION ?= 1.24.4
ORAS_VERSION ?= 1.0.0
BUILDER ?= ${BUILDER_REGISTRY}wasm-go-builder:go${GO_VERSION}-oras${ORAS_VERSION}
BUILD_TIME := $(shell date "+%Y%m%d-%H%M%S")
COMMIT_ID := $(shell git rev-parse --short HEAD 2>/dev/null)
IMAGE_TAG = $(if $(strip $(PLUGIN_VERSION)),${PLUGIN_VERSION},${BUILD_TIME}-${COMMIT_ID})
IMG ?= ${REGISTRY}${PLUGIN_NAME}:${IMAGE_TAG}
GOPROXY := $(shell go env GOPROXY)

.DEFAULT:
build:
	DOCKER_BUILDKIT=1 docker build --build-arg PLUGIN_NAME=${PLUGIN_NAME} \
	                            --build-arg BUILDER=${BUILDER}  \
	                            --build-arg GOPROXY=$(GOPROXY) \
	                            -t ${IMG} \
	                            --output extensions/${PLUGIN_NAME} \
	                            .
	@echo ""
	@echo "output wasm file: extensions/${PLUGIN_NAME}/plugin.wasm"

build-image:
	DOCKER_BUILDKIT=1 docker build --build-arg PLUGIN_NAME=${PLUGIN_NAME} \
	                            --build-arg BUILDER=${BUILDER}  \
	                            --build-arg GOPROXY=$(GOPROXY) \
	                            -t ${IMG} \
	                            .
	@echo ""
	@echo "image:            ${IMG}"

build-push: build-image
	docker push ${IMG}

local-build:
	cd extensions/${PLUGIN_NAME};GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o ./main.wasm .

	@echo ""
	@echo "wasm: extensions/${PLUGIN_NAME}/main.wasm"

