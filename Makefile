.PHONY: all build clean test fmt vet lint release debug info install docker-build

# Binary name and output directory
BINARY=nomad-driver-systemd
BIN_DIR=bin

# Build flags
BUILD_FLAGS=-ldflags="-s -w"

# Detect if we're on Linux
UNAME_S := $(shell uname -s)

all: clean fmt vet build

deps: ## Download dependencies
	@echo "Downloading dependencies..."
	@go mod download
	@go mod verify

# Native build (works on Linux, requires cross-compilation toolchain on other platforms)
build: deps
	@echo "Building $(BINARY)..."
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build $(BUILD_FLAGS) -o $(BIN_DIR)/$(BINARY) .

# Docker-based build for cross-compilation
docker-build:
	@echo "Building $(BINARY) using Docker..."
	@mkdir -p $(BIN_DIR)
	docker run --rm \
		-v $(PWD):/workspace \
		-w /workspace \
		golang:1.23-bookworm \
		sh -c "apt-get update && apt-get install -y libsystemd-dev && make build-in-docker"

build-in-docker:
	@echo "Building inside Docker container..."
	CGO_ENABLED=1 go build $(BUILD_FLAGS) -o $(BIN_DIR)/$(BINARY) .

clean:
	@echo "Cleaning..."
	@rm -rf $(BIN_DIR)
	@go clean

test:
	@echo "Running tests..."
	go test -v ./...

fmt:
	@echo "Formatting code..."
	go fmt ./...

vet:
	@echo "Running go vet..."
	@if [ "$$(uname)" = "Linux" ]; then \
		go vet ./... ; \
	else \
		echo "Skipping go vet on non-Linux platform (cross-compilation)"; \
	fi

lint:
	@echo "Running golangci-lint..."
	golangci-lint run ./...

install: build
	@echo "Installing to /opt/nomad/plugins (requires sudo)..."
	sudo mkdir -p /opt/nomad/plugins
	sudo cp $(BIN_DIR)/$(BINARY) /opt/nomad/plugins/

# Build with debug symbols for local dev
debug:
	@echo "Building $(BINARY) with debug symbols..."
	@mkdir -p $(BIN_DIR)
	GOOS=linux GOARCH=amd64 go build -gcflags="all=-N -l" -o $(BIN_DIR)/$(BINARY) .

# Release artifact (archives binary)
release: build
	@echo "Creating release artifact..."
	cd $(BIN_DIR) && tar czf $(BINARY).linux-amd64.tgz $(BINARY)

# Show build info
info:
	@echo "Go version: $$(go version)"
	@echo "Build target: linux/amd64"
	@echo "Binary: $(BIN_DIR)/$(BINARY)"
	@echo "Working directory: $$(pwd)"
