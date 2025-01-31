# Binary name
BINARY_NAME=mcpterm

# Go related variables
GOBASE=$(shell pwd)
GOBIN=$(GOBASE)/bin

# Build-time variables
BUILD_TIME=$(shell date +%FT%T%z)
GIT_COMMIT=$(shell git rev-parse --short HEAD)
VERSION?=1.0.0

# Build flags
LDFLAGS=-ldflags "-X main.Version=${VERSION} -X main.BuildTime=${BUILD_TIME} -X main.GitCommit=${GIT_COMMIT}"

# Make targets
.PHONY: all build clean test run

all: clean build

build:
	@echo "Building..."
	@go build ${LDFLAGS} -o ${GOBIN}/${BINARY_NAME} .

clean:
	@echo "Cleaning..."
	@rm -rf ${GOBIN}

test:
	@echo "Running tests..."
	@go test -v ./...

run: build
	@echo "Running..."
	@${GOBIN}/${BINARY_NAME}

# Development targets
.PHONY: dev fmt vet

dev:
	@go run main.go

fmt:
	@echo "Formatting code..."
	@go fmt ./...

vet:
	@echo "Vetting code..."
	@go vet ./...
