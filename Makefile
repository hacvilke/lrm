GO ?= go
GOBIN ?= $(shell $(GO) env GOPATH)/bin
BINARY ?= lrm
PKG ?= ./...

.PHONY: all build test vet lint clean install run-daemon help

all: build

build:
	$(GO) build -o $(BINARY) ./cmd/lrm

install:
	$(GO) install ./cmd/lrm

test:
	$(GO) test $(PKG) -count=1

test-race:
	$(GO) test -race $(PKG) -count=1

vet:
	$(GO) vet $(PKG)

fmt:
	$(GO) fmt $(PKG)

clean:
	rm -f $(BINARY) $(BINARY).exe
	$(GO) clean -testcache

# Quick local demo: init a repo in /tmp/lrm-demo
demo: build
	rm -rf /tmp/lrm-demo && mkdir -p /tmp/lrm-demo && \
	cd /tmp/lrm-demo && echo "hello LRM" > hello.txt && \
	$(abspath $(BINARY)) init && $(abspath $(BINARY)) status && \
	$(abspath $(BINARY)) commit -m "first commit" && $(abspath $(BINARY)) log

help:
	@echo "Targets: build test test-race vet fmt clean install demo"
