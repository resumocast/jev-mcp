GO ?= go
GO_VERSION := go1.27.1
GO_PACKAGES := ./cmd/... ./internal/...
NATIVE_GOARCH := $(if $(filter arm64,$(shell uname -m)),arm64,amd64)
RELEASE_VERSION ?= v0.1.1
RELEASE_LDFLAGS := -s -w -X main.version=$(RELEASE_VERSION)

.PHONY: check-go check-release-version test test-go test-pi test-binary test-export build

# Substantial builds/tests expect the pinned Go 1.27.1 toolchain (see README support matrix).
check-go:
	@test "$$($(GO) version | awk '{print $$3}')" = "$(GO_VERSION)" || \
		{ printf '%s\n' 'Use the pinned Go 1.27.1 toolchain.' >&2; exit 1; }

check-release-version:
	@printf '%s\n' '$(RELEASE_VERSION)' | grep -Eq '^[A-Za-z0-9._+-]{1,32}$$' || \
		{ printf '%s\n' 'RELEASE_VERSION must be 1-32 characters from A-Z, a-z, 0-9, ., _, +, or -.' >&2; exit 1; }

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
	@for binary in dist/jev-mcp-darwin-arm64 dist/jev-mcp-darwin-amd64; do \
		strings "$$binary" | grep -F -- '$(RELEASE_VERSION)' >/dev/null || \
		{ printf 'release version is missing from %s\n' "$$binary" >&2; exit 1; }; \
	done
	JEV_EXPECTED_VERSION="$(RELEASE_VERSION)" JEV_TEST_BINARY="$(CURDIR)/dist/jev-mcp-darwin-$(NATIVE_GOARCH)" node --import tsx --test tests/stdio-smoke.mjs tests/pi-native-smoke.ts

build: check-go check-release-version
	mkdir -p dist
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 GOTOOLCHAIN=local $(GO) build -trimpath -buildvcs=false -ldflags='$(RELEASE_LDFLAGS)' -o dist/jev-mcp-darwin-arm64 ./cmd/jev-mcp
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 GOTOOLCHAIN=local $(GO) build -trimpath -buildvcs=false -ldflags='$(RELEASE_LDFLAGS)' -o dist/jev-mcp-darwin-amd64 ./cmd/jev-mcp
	cd dist && shasum -a 256 jev-mcp-darwin-arm64 jev-mcp-darwin-amd64 > SHA256SUMS
