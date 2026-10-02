PROMETHEUS_VERSION ?= 3.5.0
HOST_OS := $(shell go env GOOS)
HOST_ARCH := $(shell go env GOARCH)

.PHONY: build test tools

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/ovnk-observ-exporter ./cmd/ovnk-observ-exporter

test:
	go test ./...

tools: bin/promtool

bin/promtool:
	mkdir -p bin
	curl -fsSL https://github.com/prometheus/prometheus/releases/download/v$(PROMETHEUS_VERSION)/prometheus-$(PROMETHEUS_VERSION).$(HOST_OS)-$(HOST_ARCH).tar.gz \
		| tar -xz -C bin --strip-components=1 prometheus-$(PROMETHEUS_VERSION).$(HOST_OS)-$(HOST_ARCH)/promtool
