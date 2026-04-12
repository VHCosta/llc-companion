BIN     = llc-companion
VERSION = 1.0.0
LDFLAGS = -ldflags="-s -w -X main.version=$(VERSION)"

.PHONY: all dev mac-arm mac-intel linux linux-arm windows clean

# Default: build for the current platform
dev:
	go build $(LDFLAGS) -o $(BIN) .

# Release: all platforms
all: mac-arm mac-intel linux linux-arm windows

mac-arm:
	GOOS=darwin  GOARCH=arm64  go build $(LDFLAGS) -o dist/$(BIN)-darwin-arm64 .

mac-intel:
	GOOS=darwin  GOARCH=amd64  go build $(LDFLAGS) -o dist/$(BIN)-darwin-amd64 .

linux:
	GOOS=linux   GOARCH=amd64  go build $(LDFLAGS) -o dist/$(BIN)-linux-amd64 .

linux-arm:
	GOOS=linux   GOARCH=arm64  go build $(LDFLAGS) -o dist/$(BIN)-linux-arm64 .

windows:
	GOOS=windows GOARCH=amd64  go build $(LDFLAGS) -o dist/$(BIN)-windows-amd64.exe .

clean:
	rm -rf dist/ $(BIN) $(BIN).exe
