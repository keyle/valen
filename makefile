# Build: `make prod` (or `make brewrelease` to tag and publish to Homebrew).
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
	@test -z "$$(git status --porcelain)" || { echo "working tree is dirty: commit or stash first"; exit 1; }; \
	upstream=$$(git rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2>/dev/null) || { echo "no upstream branch configured"; exit 1; }; \
	head=$$(git rev-parse HEAD); \
	remote=$$(git rev-parse "$$upstream" 2>/dev/null) || { echo "cannot resolve $$upstream"; exit 1; }; \
	test "$$head" = "$$remote" || { echo "HEAD is not pushed to $$upstream"; exit 1; }; \
	tagged=$$(git describe --tags --exact-match HEAD 2>/dev/null) && { echo "HEAD already tagged: $$tagged"; exit 1; }; \
	current=$$(git describe --tags --abbrev=0 2>/dev/null || echo 0.0.0); \
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

# Build release archives, publish the GitHub release and update the
# Homebrew tap (keyle/tap). Reuses the existing tag when HEAD is clean and
# fully pushed, otherwise runs `make tag`. Needs goreleaser and GITHUB_TOKEN.
brewrelease:
	@test -z "$$(git status --porcelain)" || { echo "working tree is dirty: commit or stash first"; exit 1; }; \
	upstream=$$(git rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2>/dev/null) || { echo "no upstream branch configured"; exit 1; }; \
	head=$$(git rev-parse HEAD); \
	remote=$$(git rev-parse "$$upstream" 2>/dev/null) || { echo "cannot resolve $$upstream"; exit 1; }; \
	test "$$head" = "$$remote" || { echo "HEAD is not pushed to $$upstream"; exit 1; }; \
	if tagged=$$(git describe --tags --exact-match HEAD 2>/dev/null); then \
		echo "using existing tag $$tagged"; \
	else \
		$(MAKE) tag || exit 1; \
	fi
	@command -v goreleaser >/dev/null 2>&1 || { echo "goreleaser not found: brew install goreleaser"; exit 1; }
	@test -n "$$GITHUB_TOKEN" || { echo "GITHUB_TOKEN is required (repo scope for keyle/homebrew-tap)"; exit 1; }
	goreleaser release --clean

install: prod
	@mkdir -p /usr/local/bin
	sudo install -m755 bin/valen /usr/local/bin/.

