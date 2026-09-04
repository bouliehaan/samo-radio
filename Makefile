BINARY := samo-radio
GO ?= go

.PHONY: build build-linux build-pi test vet fmt check clean devices pairing run

## build: static binary for this machine
build:
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BINARY) ./cmd/samo-radio

## build-linux: static binary for a Linux box (from a mac, usually)
## GOARCH=arm64 make build-linux for a 64-bit Pi; arm for a 32-bit one
GOARCH ?= amd64
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) $(GO) build -trimpath -o $(BINARY)-linux-$(GOARCH) ./cmd/samo-radio

## build-pi: the same thing for a Raspberry Pi running 64-bit Raspberry Pi OS
build-pi:
	$(MAKE) build-linux GOARCH=arm64

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

## devices: list the audio outputs this machine can play to
devices: build
	./$(BINARY) --devices

## pairing: print the URL and token to add this device in Samo
pairing: build
	./$(BINARY) --pairing

## run: foreground, for development
run: build
	./$(BINARY) --config ./dev-config.json

clean:
	rm -f $(BINARY) $(BINARY)-linux-*
