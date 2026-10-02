default:
	rm -rf bin/
	mkdir -p bin/
	go build .
	mv valen bin/
	cp config.toml bin/
	bin/valen
