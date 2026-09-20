GO ?= go
GO_VERSION := go1.27.1
GO_PACKAGES := ./cmd/... ./internal/...
NATIVE_GOARCH := $(if $(filter arm64,$(shell uname -m)),arm64,amd64)

.PHONY: check-go test test-go test-pi test-binary test-export build

# Substantial builds/tests expect the pinned Go 1.27.1 toolchain (see README support matrix).
check-go:
	@test "$$($(GO) version | awk '{print $$3}')" = "$(GO_VERSION)" || \
		{ printf '%s\n' 'Use the pinned Go 1.27.1 toolchain.' >&2; exit 1; }

test: test-go test-pi test-export

test-go: check-go
	@test -z "$$($(GO) fmt $(GO_PACKAGES))" || { printf '%s\n' 'Go formatting changed; review and rerun.' >&2; exit 1; }
	GOTOOLCHAIN=local $(GO) vet $(GO_PACKAGES)
	GOTOOLCHAIN=local $(GO) test -race -count=1 -timeout=120s $(GO_PACKAGES)

test-export:
	python3 -m unittest discover -s tests -p '*_test.py' -v

test-pi:
	npm run typecheck
	npm test

test-binary: build
	JEV_TEST_BINARY="$(CURDIR)/dist/jev-mcp-darwin-$(NATIVE_GOARCH)" node --import tsx --test tests/stdio-smoke.mjs tests/pi-native-smoke.ts

build: check-go
	mkdir -p dist
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 GOTOOLCHAIN=local $(GO) build -trimpath -buildvcs=false -ldflags='-s -w' -o dist/jev-mcp-darwin-arm64 ./cmd/jev-mcp
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 GOTOOLCHAIN=local $(GO) build -trimpath -buildvcs=false -ldflags='-s -w' -o dist/jev-mcp-darwin-amd64 ./cmd/jev-mcp
	cd dist && shasum -a 256 jev-mcp-darwin-arm64 jev-mcp-darwin-amd64 > SHA256SUMS
