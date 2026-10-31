# make [cli] [gui] [relay] [linux] [darwin] [windows] [VERSION=vX.Y.Z] [-j]
#
# The goals combine: `make gui linux` is the GUI for linux amd64+arm64, `make linux`
# is all three for linux, `make gui` is the GUI for this machine. No component means
# all three, no OS means this machine's. Every binary is its own target, so -j builds
# them in parallel: 5x faster on a warm Go cache. From a cold one prefer -j4, since
# each go build already uses every core and unbounded -j compiles the std lib 15
# times over. Names and flags match the release assets (publisher.yml), in .bin/.
#
# make [-j] test    unit tests (-race) and ./e2e.py; -j runs the two together

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
	GORACE=atexit_sleep_ms=0 go test -race ./...

e2e:
	./e2e.py

clean:
	rm -f $(foreach c,$(COMPONENTS),$(foreach o,$(OSES),$(foreach a,$(sort $(ARCHS_$(o))),$(call bin,$(c),$(o),$(a)))))

FORCE:

.PHONY: all $(COMPONENTS) $(OSES) test unit e2e clean FORCE
