VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-s -w -X main.version=$(VERSION)"

.PHONY: all build clean test test-integration \
	linux-amd64 linux-arm linux-arm64 linux-mipsle windows-amd64 darwin-arm64

all: linux-amd64 linux-arm linux-arm64 windows-amd64 darwin-arm64

build:
	go build $(LDFLAGS) -o mpfpv ./cmd/mpfpv/

linux-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o build/mpfpv-linux-amd64 ./cmd/mpfpv/

linux-arm:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build $(LDFLAGS) -o build/mpfpv-linux-arm ./cmd/mpfpv/

linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o build/mpfpv-linux-arm64 ./cmd/mpfpv/

linux-mipsle:
	CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build $(LDFLAGS) -o build/mpfpv-linux-mipsle ./cmd/mpfpv/

windows-amd64:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o build/mpfpv-windows-amd64.exe ./cmd/mpfpv/

darwin-arm64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o build/mpfpv-darwin-arm64 ./cmd/mpfpv/

test:
	go test ./...

test-integration:
	go test -tags integration -v ./test/...

clean:
	rm -rf build/ mpfpv mpfpv.exe
