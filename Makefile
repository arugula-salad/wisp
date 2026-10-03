GO ?= go
export WISP_DATA ?= $(HOME)/.local/share/wisp

.PHONY: all build netd deps image initrd run install-service install-sandboxd test test-scripts e2e

# The main install is NAME=wisp on the default data directory. A second daemon
# (sandboxd) goes through install-sandboxd, which needs both spelled out; and
# install-service refuses them rather than quietly reinstalling the main one.
ifneq ($(filter install-service,$(MAKECMDGOALS)),)
ifneq ($(NAME)$(DATA)$(BIN),)
$(error install-service is the main install (wisp.service, wispd, $(WISP_DATA)) and takes no NAME/DATA/BIN; for a second daemon use: make install-sandboxd NAME=<name> DATA=<dir> FLAGS='--listen ...')
endif
endif
ifneq ($(filter install-sandboxd,$(MAKECMDGOALS)),)
ifeq ($(and $(NAME),$(DATA)),)
$(error usage: make install-sandboxd NAME=<unit name, not wisp> DATA=<data dir, not wisp's> [FLAGS='--listen 127.0.0.1:7790 ...'])
endif
endif
all: build netd initrd

build:
	$(GO) build -o bin/wispd ./cmd/wispd
	$(GO) build -o bin/sandboxd ./cmd/sandboxd

netd:            ## root helper behind restrictive network policies; scripts/setup-host.sh installs it
	CGO_ENABLED=0 $(GO) build -o bin/wisp-netd ./cmd/wisp-netd

deps:            ## download firecracker + guest kernel
	./scripts/fetch-deps.sh

image:           ## build the base sprite disk image (rootless podman) into $(WISP_DATA)
	@echo "image: writing $(WISP_DATA)/images/base.ext4 (set WISP_DATA to build elsewhere)"
	./scripts/build-image.sh

initrd:          ## pack the guest agent into $(WISP_DATA); takes effect on each sprite's next cold boot
	@echo "initrd: writing $(WISP_DATA)/initrd.cpio (set WISP_DATA to build elsewhere)"
	./scripts/build-initrd.sh

run: build initrd
	./bin/wispd

install-service: build initrd   ## run wispd as a systemd user service (no root); flags: scripts/install-service.sh -- <flags>
	./scripts/install-service.sh

install-sandboxd: build   ## sandboxd as a second user service: NAME=<unit> DATA=<dir> [FLAGS='--listen ...']; never touches wisp's
	./scripts/install-service.sh --check --bin sandboxd --name $(NAME) --data $(DATA) $(if $(FLAGS),-- $(FLAGS))
	WISP_DATA=$(DATA) ./scripts/build-initrd.sh
	./scripts/install-service.sh --bin sandboxd --name $(NAME) --data $(DATA) $(if $(FLAGS),-- $(FLAGS))

test: test-scripts
	$(GO) vet ./...
	$(GO) test -race -count=1 ./...

test-scripts:    ## install-service.sh against a throwaway HOME with stubbed systemctl
	./scripts/test-install-service.sh

# The daemon under test. Both default to the main install; set them (or WISP_DATA) to
# aim at a test stack, since the environment wins over these defaults.
SPRITES_E2E_URL ?= http://127.0.0.1:7788
SPRITES_E2E_TOKEN ?= $(shell cat $(WISP_DATA)/token 2>/dev/null)
e2e:             ## official Sprites Go SDK against a running wispd (SPRITES_E2E_URL, SPRITES_E2E_TOKEN)
	SPRITES_E2E_URL=$(SPRITES_E2E_URL) SPRITES_E2E_TOKEN=$(SPRITES_E2E_TOKEN) \
	  $(GO) test -tags e2e -count=1 -v ./e2e/
