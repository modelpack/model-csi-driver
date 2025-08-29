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

.PHONY: release

release:
	@CGO_ENABLED=0 ${PROXY} GOOS=linux GOARCH=${GOARCH} go vet -tags disable_libgit2 $(PACKAGES)
	@CGO_ENABLED=0 ${PROXY} GOOS=linux GOARCH=${GOARCH} go build -tags disable_libgit2 -ldflags '${RELEASE_INFO} -w -extldflags "-static"' -o ./ ./cmd/model-csi-driver
	@CGO_ENABLED=0 ${PROXY} GOOS=linux GOARCH=${GOARCH} go build -tags disable_libgit2 -ldflags '${RELEASE_INFO} -w -extldflags "-static"' -o ./ ./cmd/model-csi-cli

test:
	@CGO_ENABLED=1 go test -tags disable_libgit2 -coverprofile cover.out.tmp -race -v -timeout 10m github.com/CloudNativeAI/model-csi-driver/pkg/server | tee coverage.log

test-local:
	go test -tags disable_libgit2 -race -c -o ./unit.test github.com/CloudNativeAI/model-csi-driver/pkg/server
	sudo CONFIG_PATH=./misc/config.test.yaml ./unit.test -test.timeout 1h -test.v -test.run ^TestServer$
