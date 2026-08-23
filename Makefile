# Hindsight. Stdlib-only Go; there is nothing to generate and nothing to install
# before building. See README.md for what the binary does once you have it.

GO     ?= go
BINARY ?= hindsight

.DEFAULT_GOAL := help
.PHONY: help build test vet fmt bench release clean

help:
	@echo 'make build     build ./$(BINARY) from ./cmd/hindsight'
	@echo 'make test      go test ./...'
	@echo 'make vet       go vet ./...'
	@echo 'make fmt       check gofmt cleanliness (does not rewrite anything)'
	@echo 'make bench     run the Go benchmarks in internal/hp'
	@echo 'make release   cross-compile release archives into dist/'
	@echo 'make clean     remove ./$(BINARY) and dist/'
	@echo
	@echo 'End-to-end hook latency is a separate thing: bash scripts/bench.sh'

build:
	$(GO) build -o $(BINARY) ./cmd/hindsight

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

# Reports, never rewrites, so it is safe to run on someone else's checkout
# and safe to gate CI on.
fmt:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo 'not gofmt-clean:'; \
		echo "$$unformatted" | sed 's/^/  /'; \
		echo 'fix with: gofmt -w .'; \
		exit 1; \
	fi; \
	echo 'gofmt clean'

bench:
	$(GO) test -run '^$$' -bench . -benchmem ./internal/hp

release:
	bash scripts/release.sh

clean:
	rm -rf dist
	rm -f $(BINARY)
