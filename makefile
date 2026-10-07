# Release: `git tag 1.0.2 && make prod`. Override with `make prod VERSION=1.0.2`.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

default:
	@rm -rf bin/
	@mkdir -p bin/
	go build -o bin/valen .
	@bin/valen

prod:
	@rm -rf bin/
	@mkdir -p bin/
	@go build -ldflags='-s -w -X "main.version=$(VERSION)"' -o bin/valen .

test:
	go test ./...

install: prod
	@mkdir -p /usr/local/bin
	install -m755 bin/valen /usr/local/bin/.

