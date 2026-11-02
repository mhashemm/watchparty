# make [cli] [gui] [relay] [linux] [darwin] [windows] [VERSION=vX.Y.Z] [-j]
#
# The goals combine: `make gui linux` is the GUI for linux amd64+arm64, `make linux`
# is all three for linux, `make gui` is the GUI for this machine. No component means
# all three, no OS means this machine's. Every binary is its own target, so -j builds
# them in parallel: 5x faster on a warm Go cache. From a cold one prefer -j4, since
# each go build already uses every core and unbounded -j compiles the std lib 15
# times over. Names and flags match the release assets (publisher.yml), in .bin/.
#
# make [-j] test    unit tests (-race) and ./e2e.py in docker; -j runs the two together

COMPONENTS := cli gui relay
OSES := linux darwin windows
ARCHS_linux := amd64 arm64
ARCHS_darwin := amd64 arm64
ARCHS_windows := amd64
HOST := $(shell go env GOOS)/$(shell go env GOARCH)
ARCHS_$(word 1,$(subst /, ,$(HOST))) += $(word 2,$(subst /, ,$(HOST)))
VERSION ?= dev
.DEFAULT_GOAL := all
OUT := .bin

pkg_cli := .
pkg_gui := ./gui
pkg_relay := ./relay
name_cli := watchparty
name_gui := watchparty-gui
name_relay := watchparty-relay

bin = $(OUT)/$(name_$(1))$(if $(filter windows,$(2)),.exe,$(if $(filter darwin,$(2)),_mac)_$(3))
ldflags = $(if $(filter gui,$(1)),-X main.version=$(VERSION) $(if $(filter windows,$(2)),-H=windowsgui))

define rule
$(call bin,$(1),$(2),$(3)): FORCE
	GOOS=$(2) GOARCH=$(3) go build -ldflags='$(call ldflags,$(1),$(2))' -o $$@ $(pkg_$(1))
endef
$(foreach c,$(COMPONENTS),$(foreach o,$(OSES),$(foreach a,$(sort $(ARCHS_$(o))),$(eval $(call rule,$(c),$(o),$(a))))))

components := $(or $(filter $(COMPONENTS),$(MAKECMDGOALS)),$(COMPONENTS))
platforms := $(or $(foreach o,$(filter $(OSES),$(MAKECMDGOALS)),$(addprefix $(o)/,$(ARCHS_$(o)))),$(HOST))
targets := $(foreach c,$(components),$(foreach p,$(platforms),$(call bin,$(c),$(word 1,$(subst /, ,$(p))),$(word 2,$(subst /, ,$(p))))))

all $(COMPONENTS) $(OSES): $(targets)

test: unit e2e

unit:
	go test -race ./...

# in e2e.Dockerfile's container as this user, so no case can reach this machine's desktop,
# bus or display. The last run's logs land in .e2e/<phase>. E2E="--mode gui" passes flags.
# It compiles into this machine's Go caches, which setup-go restores in CI, which also
# builds the image itself with a layer cache and sets E2E_PREBUILT=1.
e2e:
	$(if $(E2E_PREBUILT),,docker build -q -t watchparty-e2e --build-arg UID="$$(id -u)" --build-arg GID="$$(id -g)" - < e2e.Dockerfile > /dev/null)
	rm -rf .e2e && mkdir .e2e
	docker run --rm --init --shm-size=1g -v "$(CURDIR)":/src:ro -w /src \
		-v "$(CURDIR)/.e2e":"$(CURDIR)/.e2e" -e E2E_LOGS="$(CURDIR)/.e2e" \
		-v "$$(go env GOCACHE)":/cache/go-build -v "$$(go env GOMODCACHE)":/cache/mod watchparty-e2e ./e2e.py $(E2E)

clean:
	rm -f $(foreach c,$(COMPONENTS),$(foreach o,$(OSES),$(foreach a,$(sort $(ARCHS_$(o))),$(call bin,$(c),$(o),$(a)))))

FORCE:

.PHONY: all $(COMPONENTS) $(OSES) test unit e2e clean FORCE
