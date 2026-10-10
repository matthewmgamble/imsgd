BINARY      := bin/imsgd
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)
# Stable signing identity keeps macOS privacy (TCC) grants across rebuilds.
# Create one with scripts/create-signing-cert.sh.
SIGN_IDENTITY ?= imsgd Local Code Signing
BUNDLE_ID   := com.mgamble.imsgd
INSTALL_DIR ?= $(HOME)/.local/bin

.PHONY: build test fuzz vet lint sign install clean

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/imsgd

test:
	go test ./...

fuzz:
	go test ./internal/imessage/typedstream -run='^$$' -fuzz=FuzzDecode -fuzztime=60s

vet:
	go vet ./...

lint: vet
	@command -v golangci-lint >/dev/null && golangci-lint run || echo "golangci-lint not installed; ran go vet only"

sign: build
	codesign --force --identifier $(BUNDLE_ID) --sign "$(SIGN_IDENTITY)" $(BINARY)
	codesign --display --requirements - $(BINARY)

install: sign
	install -d $(INSTALL_DIR)
	install -m 0755 $(BINARY) $(INSTALL_DIR)/imsgd
	@echo "installed $(INSTALL_DIR)/imsgd"

clean:
	rm -rf bin dist
