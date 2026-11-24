#!/usr/bin/env bash

detect_linux_arch() {
  local os machine
  os="$(uname -s)"
  machine="$(uname -m)"

  if [[ "$os" != "Linux" ]]; then
    printf 'unsupported operating system: %s (Linux is required)\n' "$os" >&2
    return 1
  fi

  case "$machine" in
    x86_64|amd64)
      printf 'amd64\n'
      ;;
    aarch64|arm64)
      printf 'arm64\n'
      ;;
    i386|i486|i586|i686|x86)
      printf '32-bit x86 (%s) is not supported: the Android JDK/SDK builder requires a 64-bit runtime\n' "$machine" >&2
      return 1
      ;;
    armv*|arm)
      printf '32-bit ARM (%s) is not supported\n' "$machine" >&2
      return 1
      ;;
    *)
      printf 'unsupported Linux architecture: %s (supported targets: amd64, arm64)\n' "$machine" >&2
      return 1
      ;;
  esac
}
