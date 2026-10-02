default:
	rm -rf bin/
	mkdir -p bin/
	go build .
	mv mods bin/
	cp config.toml bin/
	bin/mods
