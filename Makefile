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

tidy: ## Tidy and vendor dependencies
	go mod tidy
	go mod vendor
