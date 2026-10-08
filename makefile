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

tag:
	@current=$$(git describe --tags --abbrev=0 2>/dev/null || echo 0.0.0); \
	next=$$(echo $$current | awk -F. 'BEGIN { OFS = "." } { $$NF = $$NF + 1; print }'); \
	echo "current tag: $$current"; \
	echo "next tag:    $$next"; \
	echo "building..."; \
	go build ./... || { echo "build failed"; exit 1; }; \
	echo "testing..."; \
	go test ./... || { echo "tests failed"; exit 1; }; \
	git tag $$next || exit 1; \
	git push origin $$next || exit 1; \
	echo "tagged and pushed $$next"

install: prod
	@mkdir -p /usr/local/bin
	install -m755 bin/valen /usr/local/bin/.

