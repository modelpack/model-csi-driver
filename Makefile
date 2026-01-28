PACKAGES ?= $(shell go list ./... | grep -v /vendor/)
GOARCH ?= $(shell go env GOARCH)
GOPROXY ?= https://goproxy.io

ifdef GOPROXY
PROXY := GOPROXY=${GOPROXY}
endif

# Used to populate variables in version package.
BUILD_TIMESTAMP=$(shell date '+%Y-%m-%dT%H:%M:%S')
VERSION=$(shell git describe --match 'v[0-9]*' --dirty='.m' --always --tags)
REVISION=$(shell git rev-parse HEAD)$(shell if ! git diff --no-ext-diff --quiet --exit-code; then echo .m; fi)

RELEASE_INFO = -X main.revision=${REVISION} -X main.gitVersion=${VERSION} -X main.buildTime=${BUILD_TIMESTAMP}

.PHONY: release test test-coverage

release:
	@CGO_ENABLED=0 ${PROXY} GOOS=linux GOARCH=${GOARCH} go vet -tags disable_libgit2 $(PACKAGES)
	@CGO_ENABLED=0 ${PROXY} GOOS=linux GOARCH=${GOARCH} go build -tags disable_libgit2 -ldflags '${RELEASE_INFO} -w -extldflags "-static"' -o ./ ./cmd/model-csi-driver
	@CGO_ENABLED=0 ${PROXY} GOOS=linux GOARCH=${GOARCH} go build -tags disable_libgit2 -ldflags '${RELEASE_INFO} -w -extldflags "-static"' -o ./ ./cmd/model-csi-cli

test:
	go list ./... | grep -v -E github.com/modelpack/model-csi-driver/pkg/server | xargs go test -tags disable_libgit2 -race -v -timeout 10m
	go test -tags disable_libgit2 -race -c -o ./server.test github.com/modelpack/model-csi-driver/pkg/server
	sudo CONFIG_PATH=./test/testdata/config.test.yaml ./server.test -test.v -test.timeout 10m

test-coverage:
	@echo "mode: atomic" > coverage.out
	@echo "Running tests with coverage for non-server packages..."
	@for pkg in $$(go list ./... | grep -v -E github.com/modelpack/model-csi-driver/pkg/server); do \
		go test -tags disable_libgit2 -coverprofile=coverage.tmp -covermode=atomic -timeout 10m $$pkg || exit 1; \
		if [ -f coverage.tmp ]; then \
			tail -n +2 coverage.tmp >> coverage.out; \
			rm coverage.tmp; \
		fi; \
	done
	@echo "Running tests with coverage for pkg/server..."
	@go test -tags disable_libgit2 -coverprofile=coverage.server.tmp -covermode=atomic -c -o ./server.test github.com/modelpack/model-csi-driver/pkg/server
	@sudo CONFIG_PATH=./test/testdata/config.test.yaml ./server.test -test.coverprofile=coverage.server.out -test.timeout 10m || true
	@if [ -f coverage.server.out ]; then \
		tail -n +2 coverage.server.out >> coverage.out; \
		rm coverage.server.out; \
	fi
	@rm -f coverage.server.tmp ./server.test
	@echo "Coverage report generated: coverage.out"
