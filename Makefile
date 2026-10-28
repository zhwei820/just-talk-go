.PHONY: help build install test clean run deps tidy
.DEFAULT_GOAL := help

APP_NAME := just-talk
CMD_DIR := ./cmd/just-talk
BUILD_DIR := ./build

help: ## Show available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build for current platform
	go build -o $(BUILD_DIR)/$(APP_NAME) $(CMD_DIR)

install: build ## Install to ~/.local/bin
	$(BUILD_DIR)/$(APP_NAME) --install

run: ## Run (current platform)
	go run $(CMD_DIR)

test: ## Run all tests
	go test ./... -v

clean: ## Remove build output
	rm -rf $(BUILD_DIR)

deps: ## Tidy and download dependencies
	go mod tidy
	go mod download

SHERPA_LIB_MODULES := github.com/k2-fsa/sherpa-onnx-go-macos github.com/k2-fsa/sherpa-onnx-go-linux

# go mod vendor skips sherpa-onnx's prebuilt lib/ (it holds no Go files), but
# cgo links against it, so it is copied in from the module cache.
tidy: ## Tidy and vendor dependencies
	go mod tidy
	go mod vendor
	for m in $(SHERPA_LIB_MODULES); do \
		go mod download $$m && \
		cp -R "$$(go list -mod=mod -m -f '{{.Dir}}' $$m)/lib" vendor/$$m/ && \
		chmod -R u+w vendor/$$m/lib || exit 1; \
	done
