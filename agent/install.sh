#!/bin/sh
#
# install.sh — offline-first installer for otel-php-agent
#
# Installs a prebuilt PHP extension + Go daemon **release bundle** that is
# already present on this host. No internet access is required: you obtain
# the release tarball out of band (GitHub Releases page, scp, artifact
# mirror, etc.), copy it onto the target host, and point this script at it.
#
# It then extracts the tarball (if given a .tar.gz) and runs the bundled
# `newrelic-install` script (the same agent/newrelic-install.sh the upstream
# New Relic agent uses), which:
#
#   - detects every PHP installation on the box,
#   - installs the ABI-matching newrelic.so into each PHP's extension dir,
#   - writes a newrelic.ini (from newrelic.ini.template) into each PHP's INI
#     scan directory (or prints instructions for single-php.ini systems),
#   - installs the daemon binary at /usr/bin/newrelic-daemon (or the platform
#     default), plus a platform init script, and (re)starts it.
#
# Usage:
#
#   # Offline (default — no internet needed). Point at a local tarball:
#   sudo sh install.sh ./otel-php-agent_1.0.0_linux_amd64.tar.gz install
#
#   # …or at an already-extracted bundle directory:
#   sudo sh install.sh ./otel-php-agent-linux-amd64 install
#
#   # …or let it auto-find a matching tarball in the current directory:
#   sudo sh install.sh install
#
#   # Optional: download from GitHub Releases (requires internet). Opt-in:
#   sudo INSTALL_DOWNLOAD=1 INSTALL_REPO=OWNER/otel-php-agent \
#        INSTALL_VERSION=v1.0.0 sh install.sh install
#
# Arguments after the (optional) tarball/dir path are forwarded verbatim to
# `newrelic-install` (e.g. `install`, `uninstall`, or NR_INSTALL_* knobs).
#
# Environment knobs:
#
#   INSTALL_TARBALL    Path to a local release .tar.gz (overrides the arg).
#   INSTALL_BUNDLE     Path to an already-extracted bundle dir (overrides arg).
#   INSTALL_NO_RUN=1   Extract only; print the bundle dir and exit (do not run
#                      the installer).
#
#   # Download (opt-in, requires internet):
#   INSTALL_DOWNLOAD=1 Enable the GitHub Releases download path.
#   INSTALL_REPO       GitHub "owner/repo" (e.g. OWNER/otel-php-agent).
#   INSTALL_VERSION    Release tag (e.g. v1.0.0); default "latest".
#   INSTALL_ASSET      Full asset filename override.
#   INSTALL_OS / INSTALL_ARCH / INSTALL_LIBC  Override host detection.
#

set -eu

err() { echo "install.sh: ERROR: $*" >&2; exit 1; }
log() { echo "install.sh: $*"; }
need() { command -v "$1" >/dev/null 2>&1 || err "required command not found: $1"; }

for c in tar uname rm mktemp; do need "$c"; done

#
# 1. Detect host: os / arch / libc (used for auto-finding a matching tarball
#    in the current directory, and for the opt-in download path).
#
os="$(uname -s)"
case "$os" in
  Linux)  os=linux ;;
  Darwin) os=darwin ;;
  *) err "unsupported OS: $os (this release publishes linux and darwin)" ;;
esac
os="${INSTALL_OS:-$os}"

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) err "unsupported arch: $arch (this release publishes amd64 and arm64)" ;;
esac
arch="${INSTALL_ARCH:-$arch}"

libc_suffix=""
if [ "$os" = "linux" ]; then
  if [ -n "${INSTALL_LIBC:-}" ]; then
    libc="$INSTALL_LIBC"
  elif ldd --version 2>/dev/null | grep -qi musl; then
    libc=musl
  else
    libc=gnu
  fi
  [ "$libc" = "musl" ] && libc_suffix="_musl"
fi
os_arch="${os}_${arch}${libc_suffix}"
log "host: ${os_arch}"

#
# 2. Locate the release bundle. Offline sources are preferred; the GitHub
#    download is opt-in (INSTALL_DOWNLOAD=1).
#
bundle=""        # extracted bundle dir (what newrelic-install runs from)
need_extract=0   # set if we created the bundle by extracting a tarball

resolve_tarball() {
  # $1 = explicit path arg (may be empty)
  if [ -n "${INSTALL_TARBALL:-}" ]; then
    echo "$INSTALL_TARBALL"
    return
  fi
  if [ -n "$1" ] && [ -f "$1" ]; then
    echo "$1"
    return
  fi
  # Auto-find a matching tarball in the current directory.
  for f in ./*"${os_arch}"*.tar.gz ./*"${os}"*"${arch}"*.tar.gz; do
    [ -f "$f" ] || continue
    # skip checksum sidecars
    case "$f" in *.sha256|*.sha256sum) continue ;; esac
    echo "$f"
    return
  done
  return 1
}

#
# 2a. Explicit extracted bundle dir (offline).
#
if [ -n "${INSTALL_BUNDLE:-}" ]; then
  [ -d "$INSTALL_BUNDLE" ] || err "INSTALL_BUNDLE is not a directory: $INSTALL_BUNDLE"
  bundle="$INSTALL_BUNDLE"
  log "using bundle dir: ${bundle}"
fi

#
# 2b. Positional arg or auto-found tarball (offline), unless it's an
#     installer subcommand like "install"/"uninstall".
#
if [ -z "$bundle" ]; then
  arg1="${1:-}"
  case "$arg1" in
    install|uninstall|purge|install_daemon|"") : ;; # subcommand, not a path
    *)
      if [ -d "$arg1" ]; then
        bundle="$arg1"; log "using bundle dir: ${bundle}"
      elif [ -f "$arg1" ]; then
        : # handled by resolve_tarball below
      fi
      ;;
  esac
fi

if [ -z "$bundle" ]; then
  tb="$(resolve_tarball "${1:-}")" || tb=""
  if [ -n "$tb" ] && [ -f "$tb" ]; then
    log "using tarball: ${tb}"
    dest="${INSTALL_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/otel-php-install.XXXXXX")}"
    mkdir -p "$dest"
    log "extracting to ${dest}"
    tar -xzf "$tb" -C "$dest"
    bundle="$dest"
    need_extract=1
    # If the tarball contains a single top-level dir, descend into it.
    _n=$(ls -A "$dest" 2>/dev/null | wc -l)
    if [ "$_n" -eq 1 ]; then
      _top="$(ls -A "$dest")"
      if [ -d "${dest}/${_top}" ]; then bundle="${dest}/${_top}"; fi
    fi
  fi
fi

#
# 2c. Opt-in GitHub download (requires internet). Only reached when no
#     local tarball/bundle was supplied.
#
if [ -z "$bundle" ]; then
  if [ "${INSTALL_DOWNLOAD:-0}" != "1" ]; then
    err "no release bundle found.

This installer is offline by default. Obtain the release tarball for this host
(e.g. otel-php-agent_<version>_${os_arch}.tar.gz) from GitHub Releases on a
machine with access, copy it onto this host, then either:

  sudo sh install.sh ./otel-php-agent_<version>_${os_arch}.tar.gz install

or place the tarball in the current directory and run:

  sudo sh install.sh install

If this host has internet access, opt into the download path:

  sudo INSTALL_DOWNLOAD=1 INSTALL_REPO=OWNER/otel-php-agent sh install.sh install"
  fi

  need curl
  REPO="${INSTALL_REPO:-}"
  [ -n "$REPO" ] || err "INSTALL_DOWNLOAD=1 set but INSTALL_REPO not provided (e.g. OWNER/otel-php-agent)"
  version="${INSTALL_VERSION:-latest}"
  tag="$version"
  if [ "$version" = "latest" ]; then
    log "resolving latest release for ${REPO} … (requires internet)"
    tag="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
      | sed -n 's/^[[:space:]]*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
      | head -n1)"
    [ -n "$tag" ] || err "could not resolve latest release tag from GitHub API"
  fi
  ver_num="$(printf '%s' "$tag" | sed 's/^v//')"
  asset="${INSTALL_ASSET:-otel-php-agent_${ver_num}_${os_arch}.tar.gz}"
  url="https://github.com/${REPO}/releases/download/${tag}/${asset}"
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/otel-php-install.XXXXXX")"
  log "downloading ${url} (requires internet)"
  curl -fSL --retry 3 -o "${tmp}/${asset}" "$url" \
    || err "download failed: ${url}"
  # best-effort checksum
  if curl -fsSL --retry 1 -o "${tmp}/${asset}.sha256" "${url}.sha256"; then
    ( cd "$tmp" && sha256sum -c "$(basename "${asset}.sha256")" 2>/dev/null ) \
      && log "checksum verified" || log "WARNING: checksum verification failed (continuing)"
  fi
  dest="${INSTALL_DIR:-${tmp}/bundle}"
  mkdir -p "$dest"
  tar -xzf "${tmp}/${asset}" -C "$dest"
  bundle="$dest"
  _n=$(ls -A "$dest" 2>/dev/null | wc -l)
  if [ "$_n" -eq 1 ]; then
    _top="$(ls -A "$dest")"
    if [ -d "${dest}/${_top}" ]; then bundle="${dest}/${_top}"; fi
  fi
fi

[ -d "$bundle" ] || err "bundle is not a directory: $bundle"

if [ "${INSTALL_NO_RUN:-0}" = "1" ]; then
  echo "$bundle"
  exit 0
fi

#
# 3. Locate and run the bundled installer.
#
installer="${bundle}/newrelic-install"
[ -f "$installer" ] || \
  installer="$(find "$bundle" -name newrelic-install -type f 2>/dev/null | head -n1)"
[ -f "$installer" ] || err "bundle has no newrelic-install (got: $(ls -A "$bundle"))"
[ -x "$installer" ] || chmod +x "$installer" 2>/dev/null || true

# Forward any args that are installer subcommands/knobs (skip a leading
# tarball/dir path we already consumed).
fwd=""
for a in "$@"; do
  case "$a" in
    install|uninstall|purge|install_daemon) fwd="$fwd $a" ;;
    *) [ -f "$a" ] || [ -d "$a" ] || fwd="$fwd $a" ;;
  esac
done
[ -n "$fwd" ] || fwd=" install"

log "running ${installer}${fwd}"
exec "$installer" ${fwd}