default:
	@rm -rf bin/
	@mkdir -p bin/
	@go build -o bin/valen .
	@cp config.toml bin/
	@bin/valen

prod:
	@rm -rf bin/
	@mkdir -p bin/
	@go build -ldflags='-s -w -X "main.version=1.0.1"' -o bin/valen .
	@cp config.toml bin/

