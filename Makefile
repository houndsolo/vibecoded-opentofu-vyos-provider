GO ?= go
VERSION ?= 0.1.0
BINARY := terraform-provider-vyoscmd_v$(VERSION)
MIRROR ?= $(HOME)/.local/share/terraform/providers
PLATFORM := $(shell $(GO) env GOOS)_$(shell $(GO) env GOARCH)

.PHONY: build test fmt vet check race install-local test-cli

build:
	mkdir -p bin
	$(GO) build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/$(BINARY) .

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

check: test vet
	test -z "$$($(GO) fmt ./...)"

race:
	$(GO) test -race ./...

# A short provider source resolves to a different default registry in OpenTofu.
# Install the same build under both addresses for local development.
install-local: build
	install -D -m 0755 bin/$(BINARY) $(MIRROR)/registry.terraform.io/houndsolo/vyoscmd/$(VERSION)/$(PLATFORM)/$(BINARY)
	install -D -m 0755 bin/$(BINARY) $(MIRROR)/registry.opentofu.org/houndsolo/vyoscmd/$(VERSION)/$(PLATFORM)/$(BINARY)

test-cli: build
	python3 scripts/test_cli.py --cli tofu --provider-bin bin/$(BINARY)
