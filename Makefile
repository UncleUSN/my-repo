# Сборка, тесты и кросс-компиляция ferry.
# CGO выключен: бинарник статический, на целевой машине не нужен установленный Go.

BINARY := ferry
PKG := ./cmd/ferry
VERSION ?= 0.1.0
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test race cross demo clean

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

test:
	go test ./...

race:
	go test -race ./...

cross:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/ferry-linux-amd64 $(PKG)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/ferry-linux-arm64 $(PKG)
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/ferry-windows-amd64.exe $(PKG)

demo: build
	bash scripts/demo-two-nodes.sh

clean:
	rm -rf bin dist
