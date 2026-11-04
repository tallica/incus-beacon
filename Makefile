.PHONY: check run build install test race vet lint fmt fmt-check audit release-notes release-check release-snapshot clean

BINARY   := incus-beacon
PKG      := github.com/tallica/incus-beacon
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   := $(shell git rev-parse --verify -q HEAD)
DATE     := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -X $(PKG)/internal/version.Version=$(VERSION) \
            -X $(PKG)/internal/version.Commit=$(COMMIT) \
            -X $(PKG)/internal/version.Date=$(DATE)
PRETTIER := npx --yes prettier@3.9.10

# The gate before every commit; CI also runs race tests and audit.
check: vet lint test build

run:
	go run -ldflags "$(LDFLAGS)" ./cmd/$(BINARY)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

# Into $GOBIN, ~/go/bin by default; version stamped like `build`.
install:
	CGO_ENABLED=0 go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/$(BINARY)

test:
	go test ./...

# For changes that touch goroutines, timers or shared state.
race:
	go test -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run

# Go through the formatters .golangci.yml enables, so `make lint` agrees;
# Prettier for Markdown, YAML, JSON and JS. CI runs fmt-check.
fmt:
	golangci-lint fmt
	$(PRETTIER) --write --log-level warn .

fmt-check:
	$(PRETTIER) --check --log-level warn .

# Vulnerabilities the code can reach, from the Go vulnerability database.
audit:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

release-notes:
	@test -n "$(TAG)" || { echo "usage: make release-notes TAG=vX.Y.Z" >&2; exit 2; }
	@scripts/release-notes.sh "$(TAG)"

release-check:
	goreleaser check

release-snapshot:
	goreleaser release --snapshot --clean

clean:
	rm -rf bin dist .archive
