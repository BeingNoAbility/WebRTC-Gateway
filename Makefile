VERSION := $(shell tr -d '[:space:]' < VERSION)
LDFLAGS := -s -w -buildid= -X main.Version=v$(VERSION)

.PHONY: all build build-web build-agent build-signal test test-go test-web test-conformance test-e2e lint clean

all: build

build-web:
	npm --prefix web run build

build-agent:
	mkdir -p bin
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/agent ./cmd/agent

build-signal: build-web
	mkdir -p bin
	go build -trimpath -tags embedweb -ldflags="$(LDFLAGS)" -o bin/signal ./cmd/signal

build: build-agent build-signal

test-go:
	go test -race -short ./...

test-web: build-web
	npm --prefix web test

test-conformance:
	node --test test/conformance/signaling_conformance.test.mjs

test: test-go test-web test-conformance

test-e2e: build
	SIGNAL_BIN="$(CURDIR)/bin/signal" AGENT_BIN="$(CURDIR)/bin/agent" node --test test/e2e/browser_suite.test.mjs

lint:
	@unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then echo "Files not gofmt formatted:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	node scripts/verify-release-hygiene.mjs
	npm --prefix docs run docs:lint

clean:
	rm -rf bin web/dist build_output release_assets
