# Build: `make prod` (or `make release` to tag and publish to Homebrew).
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
	echo "tidying..."; \
	go mod tidy || { echo "go mod tidy failed"; exit 1; }; \
	git diff --quiet go.mod go.sum || { echo "go mod tidy changed go.mod/go.sum: commit them first"; exit 1; }; \
	echo "linting..."; \
	test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }; \
	go vet ./... || { echo "vet failed"; exit 1; }; \
	echo "building..."; \
	go build ./... || { echo "build failed"; exit 1; }; \
	echo "testing..."; \
	go test ./... || { echo "tests failed"; exit 1; }; \
	git tag $$next || exit 1; \
	git push origin $$next || exit 1; \
	echo "tagged and pushed $$next"

# Tag, build release archives, publish the GitHub release and update the
# Homebrew tap (keyle/tap). Needs goreleaser and GITHUB_TOKEN.
release: tag
	@command -v goreleaser >/dev/null 2>&1 || { echo "goreleaser not found: brew install goreleaser"; exit 1; }
	@test -n "$$GITHUB_TOKEN" || { echo "GITHUB_TOKEN is required (repo scope for keyle/homebrew-tap)"; exit 1; }
	goreleaser release --clean

install: prod
	@mkdir -p /usr/local/bin
	sudo install -m755 bin/valen /usr/local/bin/.

