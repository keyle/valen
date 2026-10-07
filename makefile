default:
	@rm -rf bin/
	@mkdir -p bin/
	go build -o bin/valen .
	@bin/valen

prod:
	@rm -rf bin/
	@mkdir -p bin/
	@go build -ldflags='-s -w -X "main.version=1.0.1"' -o bin/valen .

test:
	go test ./...

install: prod
	@mkdir -p /usr/local/bin
	install -m755 bin/valen /usr/local/bin/.

