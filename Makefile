BINARY := samo-radio
GO ?= go

.PHONY: build test vet fmt check clean install devices run

## build: static binary for this machine
build:
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BINARY) ./cmd/samo-radio

## build-linux: static binary for the server (from a mac, usually)
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -o $(BINARY)-linux-amd64 ./cmd/samo-radio

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

## check: everything CI would run
check: vet test
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	@echo "ok"

## install: build + install the systemd service (run on the server, as root)
install:
	sudo ./packaging/install.sh

## devices: list the audio outputs this machine can play to
devices: build
	./$(BINARY) --devices

## run: foreground, for development
run: build
	./$(BINARY) --config ./dev-config.json

clean:
	rm -f $(BINARY) $(BINARY)-linux-amd64
