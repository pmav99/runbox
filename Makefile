.PHONY: build install test vet clean

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

build:
	go build $(LDFLAGS) -o runbox .

install:
	go install $(LDFLAGS) .

test:
	go test -race ./...

vet:
	go vet ./...

clean:
	rm -f runbox
	rm -rf .runbox
