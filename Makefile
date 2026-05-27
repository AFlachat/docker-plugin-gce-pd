# Makefile for docker-plugin-gce-pd
#
# Packaging targets (build/rootfs/plugin-create/plugin-push) run anywhere with a
# Docker daemon — they are pure packaging and never touch GCE. Only running the
# plugin (plugin-enable on a host) requires a GCE VM, since startup reconciles
# against the metadata server and the Compute API.

# ---- configuration (override on the command line) ---------------------------
REGISTRY    ?= ghcr.io/aflachat
PLUGIN_NAME ?= docker-plugin-gce-pd
TAG         ?= latest

# Target architecture for the plugin. Managed plugins are single-arch, so each
# architecture is published under its own tag suffix (e.g. :latest-amd64).
# ARCH uses Docker/Go naming: amd64, arm64.
ARCH        ?= amd64

# ARCHES is the set the `release` target iterates over.
ARCHES      ?= amd64 arm64

# PLUGIN is the arch-suffixed reference actually built and pushed.
PLUGIN      := $(REGISTRY)/$(PLUGIN_NAME):$(TAG)-$(ARCH)

# Intermediate image used only to export the plugin rootfs (also arch-scoped so
# concurrent multi-arch builds don't clobber each other).
ROOTFS_IMAGE := $(PLUGIN_NAME)-rootfs:$(TAG)-$(ARCH)

# Local build dir assembled into the managed plugin (rootfs/ + config.json).
# Arch-scoped for the same reason.
PLUGIN_DIR := build/plugin-$(ARCH)

GO          ?= go
GOFLAGS     ?=

.DEFAULT_GOAL := build

# ---- Go targets -------------------------------------------------------------
.PHONY: build
build: ## Compile the static plugin binary into ./bin (honours ARCH)
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) $(GO) build -trimpath -ldflags="-s -w" \
		-o bin/docker-volume-gcepd ./cmd/docker-volume-gcepd

.PHONY: test
test: ## Run unit tests (race detector + coverage)
	$(GO) test -race -coverprofile=coverage.txt ./...

.PHONY: test-short
test-short: ## Run unit tests without the race detector
	$(GO) test ./...

.PHONY: lint
lint: ## Run go vet and gofmt check
	$(GO) vet ./...
	@unformatted=$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*')); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: tidy
tidy: ## Sync go.mod/go.sum
	$(GO) mod tidy

# ---- managed-plugin packaging ----------------------------------------------
.PHONY: rootfs
rootfs: ## Build the rootfs image and assemble the plugin dir (honours ARCH)
	docker build --platform=linux/$(ARCH) -t $(ROOTFS_IMAGE) .
	rm -rf $(PLUGIN_DIR)
	mkdir -p $(PLUGIN_DIR)/rootfs
	cp config.json $(PLUGIN_DIR)/config.json
	# Export the image filesystem into rootfs/ via a throwaway container.
	cid=$$(docker create --platform=linux/$(ARCH) $(ROOTFS_IMAGE)); \
	docker export "$$cid" | tar -x -C $(PLUGIN_DIR)/rootfs; \
	docker rm -f "$$cid" >/dev/null

.PHONY: plugin-create
plugin-create: rootfs ## Create the managed plugin from build/plugin
	docker plugin rm -f $(PLUGIN) 2>/dev/null || true
	docker plugin create $(PLUGIN) $(PLUGIN_DIR)

.PHONY: plugin-push
plugin-push: plugin-create ## Push the (arch-suffixed) managed plugin to the registry
	docker plugin push $(PLUGIN)

.PHONY: release
release: ## Build + push the plugin for every arch in ARCHES (default: amd64 arm64)
	@for a in $(ARCHES); do \
		echo ">>> releasing $(REGISTRY)/$(PLUGIN_NAME):$(TAG)-$$a"; \
		$(MAKE) plugin-push ARCH=$$a TAG=$(TAG) REGISTRY=$(REGISTRY) PLUGIN_NAME=$(PLUGIN_NAME) || exit 1; \
	done

.PHONY: plugin-enable
plugin-enable: ## Enable the plugin (GCE VM only — fails off-GCE by design)
	docker plugin enable $(PLUGIN)

.PHONY: plugin-disable
plugin-disable: ## Disable the plugin
	docker plugin disable $(PLUGIN) || true

.PHONY: plugin-rm
plugin-rm: ## Remove the managed plugin
	docker plugin rm -f $(PLUGIN) 2>/dev/null || true

# ---- housekeeping -----------------------------------------------------------
.PHONY: clean
clean: ## Remove build artifacts and the rootfs image
	rm -rf bin build coverage.txt
	docker rmi $(ROOTFS_IMAGE) 2>/dev/null || true

.PHONY: help
help: ## List available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
