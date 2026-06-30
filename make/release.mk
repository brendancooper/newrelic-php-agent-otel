#
# Copyright 2020 New Relic Corporation. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#

#
# Build recipes for constructing an agent release with build machines.
#
# Note: The recipes in this file assume the current working directory is
# the top-level of the project.
#
# The release bundle produced here is the self-contained directory that
# agent/install.sh (and the bundled newrelic-install) consume. Its layout,
# relative to the bundle root (the directory containing newrelic-install),
# is:
#
#   newrelic-install            # platform-independent shell installer
#   VERSION                     # agent version + git commit
#   COMMIT
#   README.txt                  # upstream agent README
#   LICENSE
#   daemon/newrelic-daemon.<arch>                # Go daemon binary
#   agent/<arch>/newrelic-<PHP_API_VERSION>[-zts].so   # one per PHP
#   scripts/                    # init scripts, templates, newrelic-iutil.<arch>
#
#   otel-php-agent_<version>_<os>_<arch>[_musl].tar.gz   ← release asset
#   otel-php-agent_<version>_<os>_<arch>[_musl].tar.gz.sha256
#
# RELEASE_OS / RELEASE_ARCH may be overridden on the command line (or in the
# environment) to pin a target from a cross-build / CI runner that does not
# match the build machine (e.g. build the musl bundle on a glibc host, or
# assemble on a host after the per-PHP .so files were staged inside builder
# containers). The internal arch names are the New Relic ones (x64, aarch64,
# x86_64 on osx); the *asset* arch names are the portable ones (amd64, arm64)
# used by agent/install.sh and the README.
#

# RELEASE_OS / RELEASE_ARCH are overridable. Only auto-detect when unset so
# that CI build matrices can pin an exact (os, libc, arch) independently of
# the runner that runs `make release-tarball`.
ifndef RELEASE_OS
  _RELEASE_OS := $(shell uname -s | tr '[:upper:]' '[:lower:]')
  _RELEASE_OS := $(_RELEASE_OS:darwin=osx)
  _RELEASE_OS := $(_RELEASE_OS:sunos=solaris)

  # Detect whether this build will link against musl libc. For now, this
  # check assumes musl is the only libc that does not define a special
  # symbol to uniquely identify it. If necessary, this check can be easily
  # expanded to cover other C standard library implementations such as
  # uclibc or dietlibc.
  ifeq (linux,$(_RELEASE_OS))
    RELEASE_LIBC := $(shell $(CC) -x c -E make/detect-linux-libc.env | grep -E '^LIBC.*=')
    ifeq (gnu,$(findstring gnu,$(RELEASE_LIBC)))
      RELEASE_OS := linux
    else ifeq (musl,$(findstring musl,$(RELEASE_LIBC)))
      RELEASE_OS := linux-musl
    else
      $(error Cannot detect the C library in use; set RELEASE_OS explicitly)
    endif
  else
    RELEASE_OS := $(_RELEASE_OS)
  endif
endif

RELEASE_ARCH ?= $(ARCH)

# Darwin uses a custom architecture name. This is a remnant of the switch
# from universal to x86_64 only binaries.
ifeq (osx,$(RELEASE_OS))
  ifeq (x64,$(RELEASE_ARCH))
    RELEASE_ARCH := x86_64
  endif
endif

#
# Asset naming. The release tarball uses portable os/arch names and a _musl
# libc suffix, matching agent/install.sh auto-find logic and the README:
#
#   otel-php-agent_<version>_<os>_<arch>[_musl].tar.gz
#
# e.g. otel-php-agent_1.0.0_linux_amd64.tar.gz
#      otel-php-agent_1.0.0_linux_arm64_musl.tar.gz
#
# RELEASE_VERSION defaults to AGENT_VERSION (which carries a BUILD_NUMBER
# suffix for CI); a release workflow pins it to the bare tag so the asset
# name matches what installer download mode reconstructs from the tag.
RELEASE_VERSION ?= $(AGENT_VERSION)

# Internal bundle os-name -> portable asset os-name (osx -> darwin).
RELEASE_ASSET_OS := $(RELEASE_OS:osx=darwin)
# linux vs linux-musl: drop the -musl suffix from the os segment; it goes
# into the libc suffix appended after arch.
RELEASE_ASSET_OS := $(RELEASE_ASSET_OS:linux-musl=linux)

# Internal bundle arch-name -> portable asset arch-name.
#   x64 / x86_64 -> amd64 ; aarch64 -> arm64
RELEASE_ASSET_ARCH := $(RELEASE_ARCH:x64=amd64)
RELEASE_ASSET_ARCH := $(RELEASE_ASSET_ARCH:x86_64=amd64)
RELEASE_ASSET_ARCH := $(RELEASE_ASSET_ARCH:aarch64=arm64)

# libc suffix for the asset (and bundle-dir) name: "_musl" if musl, else "".
RELEASE_LIBC_SUFFIX :=
ifeq (linux-musl,$(RELEASE_OS))
  RELEASE_LIBC_SUFFIX := _musl
endif

ASSET_NAME := otel-php-agent_$(RELEASE_VERSION)_$(RELEASE_ASSET_OS)_$(RELEASE_ASSET_ARCH)$(RELEASE_LIBC_SUFFIX)
# Extracted single top-level directory name inside the tarball, matching the
# README's "already-extracted bundle dir" examples (otel-php-agent-linux-amd64).
BUNDLE_DIR := otel-php-agent-$(RELEASE_ASSET_OS)-$(RELEASE_ASSET_ARCH)$(RELEASE_LIBC_SUFFIX)

.PHONY: release
# Full local one-shot: build agent (host arch/libc) + bundle the rest. CI does
# NOT use `release`; it pre-stages the agent .so files inside per-PHP builder
# containers, then calls `release-tarball` (which only depends on
# `release-bundle`, so it never rebuilds the agent on the assemble host).
release: agent installer daemon release-bundle | releases/$(RELEASE_OS)/

# Everything in the bundle *except* the per-PHP agent .so files. These recipes
# only stage already-built artifacts (newrelic-install, newrelic-iutil,
# bin/daemon) so a release assemble never recompiles a host-mismatched binary.
.PHONY: release-bundle
release-bundle: release-version release-installer release-daemon release-docs release-scripts | releases/$(RELEASE_OS)/

release-version: releases/$(RELEASE_OS)/
	printf '%s\n' "$(AGENT_VERSION)" > releases/$(RELEASE_OS)/VERSION
	printf '%s\n' "$(GIT_COMMIT)" > releases/$(RELEASE_OS)/COMMIT

# Copies the prebuilt daemon (built with the matching target libc/arch, e.g.
# inside a builder container). We deliberately do NOT depend on the `daemon`
# target here: rebuilding on the assemble host could overwrite a musl/arm64
# daemon with a host-matched one.
release-daemon: | releases/$(RELEASE_OS)/daemon/
	@test -f bin/daemon || { echo "ERROR: bin/daemon not built (run 'make daemon' for the target first)"; exit 1; }
	@echo "staging daemon -> releases/$(RELEASE_OS)/daemon/newrelic-daemon.$(RELEASE_ARCH)"
	cp bin/daemon releases/$(RELEASE_OS)/daemon/newrelic-daemon.$(RELEASE_ARCH)

.PHONY: release-installer
release-installer: release-installer-script release-installer-iutil

# Copies the prebuilt newrelic-install (a sed-substituted shell script; not
# libc-specific, but built via `make installer`).
release-installer-script: | releases/$(RELEASE_OS)/
	@test -f bin/newrelic-install || { echo "ERROR: bin/newrelic-install not built (run 'make installer')"; exit 1; }
	cp bin/newrelic-install releases/$(RELEASE_OS)

# Copies the prebuilt newrelic-iutil (a C helper; libc-arch-specific, so it
# must be built on a matching target — not on the assemble host.
release-installer-iutil: | releases/$(RELEASE_OS)/scripts/
	@test -f bin/newrelic-iutil || { echo "ERROR: bin/newrelic-iutil not built (run 'make installer')"; exit 1; }
	cp bin/newrelic-iutil   releases/$(RELEASE_OS)/scripts/newrelic-iutil.$(RELEASE_ARCH)

release-docs: Makefile | releases/$(RELEASE_OS)/
	cp agent/README.txt LICENSE releases/$(RELEASE_OS)

release-scripts: Makefile | releases/$(RELEASE_OS)/scripts/
	cp agent/scripts/init.alpine               releases/$(RELEASE_OS)/scripts
	cp agent/scripts/init.darwin               releases/$(RELEASE_OS)/scripts
	cp agent/scripts/init.debian               releases/$(RELEASE_OS)/scripts
	cp agent/scripts/init.freebsd              releases/$(RELEASE_OS)/scripts
	cp agent/scripts/init.generic              releases/$(RELEASE_OS)/scripts
	cp agent/scripts/init.rhel                 releases/$(RELEASE_OS)/scripts
	cp agent/scripts/init.solaris              releases/$(RELEASE_OS)/scripts
	cp agent/scripts/newrelic-daemon.logrotate releases/$(RELEASE_OS)/scripts
	cp agent/scripts/newrelic-daemon.service   releases/$(RELEASE_OS)/scripts
	cp agent/scripts/newrelic-php5.logrotate   releases/$(RELEASE_OS)/scripts
	cp agent/scripts/newrelic.cfg.template     releases/$(RELEASE_OS)/scripts
	cp agent/scripts/newrelic.ini.template     releases/$(RELEASE_OS)/scripts
	cp agent/scripts/newrelic.sysconfig        releases/$(RELEASE_OS)/scripts
	cp agent/scripts/newrelic.xml              releases/$(RELEASE_OS)/scripts

#
# GitHub Actions release target - release-agent. GitHub Actions release
# workflow builds extension binaries in parallel with dedicated build
# service for each supported PHP version.
#
release-agent: Makefile | releases/$(RELEASE_OS)/agent/$(RELEASE_ARCH)/
	@$(MAKE) agent-clean && $(MAKE) agent-for-release

# Older versions of GNU Make had a bug where "#" in a function invocation
# such as $(shell ...) was treated as a make comment. This makefile needs
# to be compatible with older versions of GNU Make, so we need to use
# a workaround by assigning "#" to a variable and using that variable in
# the function invocation.
H := \#

# Target for building the agent for a given PHP version. Works well
# when building agent using containers. This is useful not only in
# GitHub Actions workflows, but also in a day to day development,
# because it allows to preserve agent between PHP version switches.
agent-for-release: PHP_API_VERSION=$(shell awk '/^$(H)define[[:space:]]+PHP_API_VERSION/ {print $$3}' "$(shell $(PHP_CONFIG) --include-dir)/main/php.h")
agent-for-release: PHP_ZTS=$(shell awk '/^$(H)define[[:space:]]+ZTS/ {print "-zts"}' "$(shell $(PHP_CONFIG) --include-dir)/main/php_config.h")
agent-for-release: Makefile agent | releases/$(RELEASE_OS)/agent/$(RELEASE_ARCH)/
	@test -n "$(PHP_API_VERSION)" || { echo "ERROR: Could not detect PHP_API_VERSION"; exit 1; }
	@echo "PHP API version detected: [$(PHP_API_VERSION)]"
	@echo "PHP variant detected: [$(PHP_ZTS)]"
	@cp -v agent/modules/newrelic.so "releases/$(RELEASE_OS)/agent/$(RELEASE_ARCH)/newrelic-$(PHP_API_VERSION)$(PHP_ZTS).so"
	@test -e agent/newrelic.map && cp -v agent/newrelic.map "releases/$(RELEASE_OS)/agent/$(RELEASE_ARCH)/newrelic-$(PHP_API_VERSION)$(PHP_ZTS).map" || true

#
# Package the staged bundle into the GitHub Releases asset. CI pre-stages the
# per-PHP agent .so files inside builder containers (one per PHP version) and
# the daemon/installer built for the matching target, then calls this. The
# tarball contains a single top-level dir (BUNDLE_DIR) so agent/install.sh can
# descend into it, plus a sha256 sidecar (named <asset>.sha256, matching
# install.sh's download-verify path).
#
.PHONY: release-tarball
release-tarball: release-bundle | releases/
	@rm -rf releases/$(BUNDLE_DIR)
	@cp -a releases/$(RELEASE_OS) releases/$(BUNDLE_DIR)
	@rm -f releases/$(ASSET_NAME).tar.gz releases/$(ASSET_NAME).tar.gz.sha256
	cd releases && tar -czf $(ASSET_NAME).tar.gz $(BUNDLE_DIR)
	cd releases && sha256sum $(ASSET_NAME).tar.gz > $(ASSET_NAME).tar.gz.sha256
	@echo "release: releases/$(ASSET_NAME).tar.gz (+ .sha256)"

#
# Release directories
#

releases/:
	mkdir releases

releases/$(RELEASE_OS)/: Makefile | releases/
	mkdir releases/$(RELEASE_OS)

releases/$(RELEASE_OS)/agent/: Makefile | releases/$(RELEASE_OS)/
	mkdir releases/$(RELEASE_OS)/agent

releases/$(RELEASE_OS)/agent/$(RELEASE_ARCH)/: Makefile | releases/$(RELEASE_OS)/agent/
	mkdir releases/$(RELEASE_OS)/agent/$(RELEASE_ARCH)

releases/$(RELEASE_OS)/daemon/: Makefile | releases/$(RELEASE_OS)/
	mkdir releases/$(RELEASE_OS)/daemon

releases/$(RELEASE_OS)/scripts/: Makefile | releases/$(RELEASE_OS)/
	mkdir releases/$(RELEASE_OS)/scripts