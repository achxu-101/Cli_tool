.PHONY: build install clean lint

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o upgrador .

install: build
	install -m 0755 upgrador /usr/local/bin/upgrador

clean:
	rm -f upgrador

lint:
	golangci-lint run ./...
