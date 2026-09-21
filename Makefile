GO ?= go
export MINI_SPRITES_DATA ?= $(HOME)/.local/share/mini-sprites

.PHONY: all build deps image initrd run test e2e
all: build initrd

build:
	$(GO) build -o bin/spritesd ./cmd/spritesd

deps:            ## download firecracker + guest kernel
	./scripts/fetch-deps.sh

image:           ## build the base sprite disk image (rootless podman)
	./scripts/build-image.sh

initrd:          ## pack the guest agent; takes effect on each sprite's next cold boot
	./scripts/build-initrd.sh

run: build initrd
	./bin/spritesd

test:
	$(GO) vet ./...
	$(GO) test -race -count=1 ./...

e2e:             ## official Sprites Go SDK against a running spritesd
	SPRITES_E2E_URL=http://127.0.0.1:7788 SPRITES_E2E_TOKEN=$$(cat $(MINI_SPRITES_DATA)/token) \
	  $(GO) test -tags e2e -count=1 -v ./e2e/
