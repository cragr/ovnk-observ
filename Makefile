PROMETHEUS_VERSION ?= 3.5.0
HOST_OS := $(shell go env GOOS)
HOST_ARCH := $(shell go env GOARCH)

NODE ?= <worker-node>

.PHONY: build test tools image deploy undeploy verify-counts integration

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/ovnk-observ-exporter ./cmd/ovnk-observ-exporter

test:
	go test ./...

tools: bin/promtool

bin/promtool:
	mkdir -p bin
	curl -fsSL https://github.com/prometheus/prometheus/releases/download/v$(PROMETHEUS_VERSION)/prometheus-$(PROMETHEUS_VERSION).$(HOST_OS)-$(HOST_ARCH).tar.gz \
		| tar -xz -C bin --strip-components=1 prometheus-$(PROMETHEUS_VERSION).$(HOST_OS)-$(HOST_ARCH)/promtool

# Upload only the Dockerfile and binary, not the whole repo (bin/promtool is ~150MB).
image: build
	@ctx=$$(mktemp -d) && trap 'rm -rf "$$ctx"' EXIT && \
		mkdir -p "$$ctx/bin" && cp Dockerfile "$$ctx/" && cp bin/ovnk-observ-exporter "$$ctx/bin/" && \
		oc start-build ovnk-observ-exporter -n ovnk-observ --from-dir="$$ctx" --follow

deploy:
	oc apply -k deploy/

undeploy:
	oc delete -k deploy/ --ignore-not-found

verify-counts:
	hack/verify-counts.sh $(NODE)

integration:
	hack/integration/run.sh
