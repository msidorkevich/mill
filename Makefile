VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X mill/internal/cli.Version=$(VERSION)
# The mill gate passes GOTESTFLAGS=-count=1 so a cached test pass never stands in for a run.
GOTESTFLAGS ?=

.PHONY: check fmt vet test build install clean

# The gate for mill itself: formatting, vet, tests. Zero tokens, no network.
check: fmt vet test

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

test:
	go test $(GOTESTFLAGS) ./...

build:
	go build -ldflags "$(LDFLAGS)" -o bin/mill ./cmd/mill

# Installs the binary you run day to day. Only install from a merged main (dogfood rule 1).
install:
	go install -ldflags "$(LDFLAGS)" ./cmd/mill

clean:
	rm -rf bin
