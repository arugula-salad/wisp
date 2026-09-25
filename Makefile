GO ?= go
export WISP_DATA ?= $(HOME)/.local/share/wisp

.PHONY: all build netd deps image initrd run install-service test e2e
all: build netd initrd

build:
	$(GO) build -o bin/wispd ./cmd/wispd

netd:            ## root helper behind restrictive network policies; scripts/setup-host.sh installs it
	CGO_ENABLED=0 $(GO) build -o bin/wisp-netd ./cmd/wisp-netd

deps:            ## download firecracker + guest kernel
	./scripts/fetch-deps.sh

image:           ## build the base sprite disk image (rootless podman)
	./scripts/build-image.sh

initrd:          ## pack the guest agent; takes effect on each sprite's next cold boot
	./scripts/build-initrd.sh

run: build initrd
	./bin/wispd

install-service: build initrd   ## run wispd as a systemd user service (no root); flags: scripts/install-service.sh -- <flags>
	./scripts/install-service.sh

test:
	$(GO) vet ./...
	$(GO) test -race -count=1 ./...

e2e:             ## official Sprites Go SDK against a running wispd
	SPRITES_E2E_URL=http://127.0.0.1:7788 SPRITES_E2E_TOKEN=$$(cat $(WISP_DATA)/token) \
	  $(GO) test -tags e2e -count=1 -v ./e2e/
