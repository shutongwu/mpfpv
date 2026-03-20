VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-s -w -X main.version=$(VERSION)"

.PHONY: all build clean test test-integration \
	server client client-x86 client-arm client-arm32 client-windows client-darwin client-mipsle \
	linux-amd64 linux-arm linux-arm64 linux-mipsle windows-amd64 darwin-arm64

# === Top-level targets ===
all: server client

build:
	go build $(LDFLAGS) -o mpfpv ./cmd/mpfpv/

# === Server (Linux amd64 only) ===
server:
	mkdir -p build/server
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o build/server/mpfpv-linux-amd64 ./cmd/mpfpv/

# === Client targets ===
client: client-x86 client-arm client-windows

client-x86:
	mkdir -p build/client/x86
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o build/client/x86/mpfpv-linux-amd64 ./cmd/mpfpv/

client-arm:
	mkdir -p build/client/arm
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o build/client/arm/mpfpv-linux-arm64 ./cmd/mpfpv/

client-arm32:
	mkdir -p build/client/arm
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build $(LDFLAGS) -o build/client/arm/mpfpv-linux-arm ./cmd/mpfpv/

client-windows:
	mkdir -p build/client/windows
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o build/client/windows/mpfpv-windows-amd64.exe ./cmd/mpfpv/

client-darwin:
	mkdir -p build/client/darwin
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o build/client/darwin/mpfpv-darwin-arm64 ./cmd/mpfpv/

client-mipsle:
	mkdir -p build/client/mipsle
	CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build $(LDFLAGS) -o build/client/mipsle/mpfpv-linux-mipsle ./cmd/mpfpv/

# === Backward-compatible aliases ===
linux-amd64: server client-x86
linux-arm: client-arm32
linux-arm64: client-arm
linux-mipsle: client-mipsle
windows-amd64: client-windows
darwin-arm64: client-darwin

# === Test ===
test:
	go test ./...

test-integration:
	go test -tags integration -v ./test/...

# === Clean ===
clean:
	rm -rf build/ mpfpv mpfpv.exe
