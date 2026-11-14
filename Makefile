VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test dist docker clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o push ./cmd/push

test:
	go vet ./...
	go test ./...

dist:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/push-linux-amd64       ./cmd/push
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/push-linux-arm64       ./cmd/push
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/push-windows-amd64.exe ./cmd/push

docker:
	docker build --build-arg VERSION=$(VERSION) -t push-unaware-serving-host:$(VERSION) .

clean:
	rm -rf push push.exe dist
