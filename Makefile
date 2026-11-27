VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/msidorkevich/mill/internal/cli.Version=$(VERSION)
# The mill gate passes GOTESTFLAGS=-count=1 so a cached test pass never stands in for a run.
GOTESTFLAGS ?=

.PHONY: check fmt vet test flaky build install clean

# The gate for mill itself: formatting, vet, tests. Zero tokens, no network.
check: fmt vet test

# Every git a test starts runs without automatic maintenance or gc, so none leaves a
# detached process behind (the same settings internal/testenv.DisableGitMaintenance adds).
check test flaky: export GIT_CONFIG_COUNT = 2
check test flaky: export GIT_CONFIG_KEY_0 = maintenance.auto
check test flaky: export GIT_CONFIG_VALUE_0 = false
check test flaky: export GIT_CONFIG_KEY_1 = gc.auto
check test flaky: export GIT_CONFIG_VALUE_1 = 0

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

test:
	go test $(GOTESTFLAGS) ./...

# Repeats every test three times under the race detector to expose flaky tests. Not part of check.
flaky:
	go test -count=3 -race ./...

build:
	go build -ldflags "$(LDFLAGS)" -o bin/mill ./cmd/mill

# Installs the binary you run day to day. Only install from a merged main (dogfood rule 1).
install:
	go install -ldflags "$(LDFLAGS)" ./cmd/mill

clean:
	rm -rf bin
