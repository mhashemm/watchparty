# make [cli] [gui] [relay] [linux] [darwin] [windows] [VERSION=vX.Y.Z] [-j]
#
# The goals combine: `make gui linux` is the GUI for linux amd64+arm64, `make linux`
# is all three for linux, `make gui` is the GUI for this machine. No component means
# all three, no OS means this machine's. Every binary is its own target, so -j builds
# them in parallel: 5x faster on a warm Go cache. From a cold one prefer -j4, since
# each go build already uses every core and unbounded -j compiles the std lib 15
# times over. Names and flags match the release assets (publisher.yml), in .bin/.
#
# make setup        the Windows installer, from gui/setup.nsi (needs makensis)
# make fonts        Noto Color Emoji, which the GUI embeds: fetched, not in git (10 MB)
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
ldflags = $(strip -s -w $(if $(filter cli gui,$(1)),-X main.version=$(VERSION)) $(if $(filter gui,$(1)),$(if $(filter windows,$(2)),-H=windowsgui)))

# a pinned release, checked by its SHA-256; bump the tag and the sum together
NOTO := gui/fonts/NotoColorEmoji.ttf
NOTO_URL := https://raw.githubusercontent.com/googlefonts/noto-emoji/v2026-09-24-unicode18_0/2D/fonts/NotoColorEmoji.ttf
NOTO_SHA256 := 15671215ab769fdc7162a045d56fd7d7e477c51b04e6b3c761d914d8fdd6cc44

fonts: $(NOTO)

$(NOTO):
	curl -fsSLo $@.part $(NOTO_URL)
	echo "$(NOTO_SHA256)  $@.part" | shasum -a 256 -c -
	mv $@.part $@

define rule
$(call bin,$(1),$(2),$(3)): FORCE $(if $(filter gui,$(1)),$(NOTO))
	CGO_ENABLED=0 GOOS=$(2) GOARCH=$(3) go build -ldflags='$(call ldflags,$(1),$(2))' -o $$@ $(pkg_$(1))
endef
$(foreach c,$(COMPONENTS),$(foreach o,$(OSES),$(foreach a,$(sort $(ARCHS_$(o))),$(eval $(call rule,$(c),$(o),$(a))))))

components := $(or $(filter $(COMPONENTS),$(MAKECMDGOALS)),$(COMPONENTS))
platforms := $(or $(foreach o,$(filter $(OSES),$(MAKECMDGOALS)),$(addprefix $(o)/,$(ARCHS_$(o)))),$(HOST))
targets := $(foreach c,$(components),$(foreach p,$(platforms),$(call bin,$(c),$(word 1,$(subst /, ,$(p))),$(word 2,$(subst /, ,$(p))))))

all $(COMPONENTS) $(OSES): $(targets)

setup: $(OUT)/watchparty-gui-setup.exe

$(OUT)/watchparty-gui-setup.exe: $(OUT)/watchparty-gui.exe
	makensis -V2 -DEXE=$(CURDIR)/$< -DOUT=$(CURDIR)/$@ -DVERSION=$(VERSION) gui/setup.nsi

test: unit e2e

unit: $(NOTO)
	go test -race -tags disable_libutp ./...

# in e2e.Dockerfile's container as this user, so no case can reach this machine's desktop,
# bus or display. The last run's logs land in .e2e/<phase>. E2E="--mode gui" passes flags.
# It compiles into this machine's Go caches, which setup-go restores in CI. CI builds the
# image beforehand, from a layer cache kept between runs, and says so with E2E_IMAGE_BUILT=1.
e2e:
ifndef E2E_IMAGE_BUILT
	docker build -q -t watchparty-e2e --build-arg UID="$$(id -u)" --build-arg GID="$$(id -g)" - < e2e.Dockerfile > /dev/null
endif
	rm -rf .e2e && mkdir .e2e
	docker run --rm --init --shm-size=1g -v "$(CURDIR)":/src:ro -w /src \
		-v "$(CURDIR)/.e2e":"$(CURDIR)/.e2e" -e E2E_LOGS="$(CURDIR)/.e2e" \
		-v "$$(go env GOCACHE)":/cache/go-build -v "$$(go env GOMODCACHE)":/cache/mod watchparty-e2e ./e2e.py $(E2E)

clean:
	rm -f $(OUT)/watchparty-gui-setup.exe $(foreach c,$(COMPONENTS),$(foreach o,$(OSES),$(foreach a,$(sort $(ARCHS_$(o))),$(call bin,$(c),$(o),$(a)))))

FORCE:

.PHONY: all $(COMPONENTS) $(OSES) setup fonts test unit e2e clean FORCE
