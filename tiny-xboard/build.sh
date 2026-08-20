#!/bin/sh
# tiny-xboard build script (runs on the DEVELOPER machine, never on servers).
#
#   ./build.sh            build linux/amd64 + linux/arm64, write SHA256SUMS
#   ./build.sh amd64      build only linux/amd64
#   ./build.sh arm64      build only linux/arm64
#   ./build.sh all        same as no argument
#
# Outputs:
#   bin/tiny-xboard-linux-amd64
#   bin/tiny-xboard-linux-arm64
#   checksums/SHA256SUMS
#
# Overridable via environment (no git needed):
#   VERSION COMMIT BUILD_TIME
set -eu

cd "$(dirname "$0")"

APP="tiny-xboard"
GO="${GO:-go}"

command -v "$GO" >/dev/null 2>&1 || { echo "ERROR: go not found (GO=$GO)" >&2; exit 1; }

# --- version metadata -----------------------------------------------------
VERSION="${VERSION:-dev}"
COMMIT="${COMMIT:-}"
BUILD_TIME="${BUILD_TIME:-}"
if [ -z "$COMMIT" ] && command -v git >/dev/null 2>&1 && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  COMMIT="$(git rev-parse --short HEAD 2>/dev/null || true)"
fi
[ -n "$COMMIT" ] || COMMIT="unknown"
[ -n "$BUILD_TIME" ] || BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

LDFLAGS="-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.buildTime=$BUILD_TIME"

# --- helpers --------------------------------------------------------------
sha256_() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1"
  else echo "ERROR: need sha256sum or shasum" >&2; exit 1
  fi
}

elf_machine() {
  # Read ELF e_machine (offset 18, 2 bytes little-endian) via od.
  # 62 (0x3e) = x86-64, 183 (0xb7) = AArch64.
  od -An -tu1 -j18 -N2 "$1" 2>/dev/null | awk '{printf "%d\n", $1 + $2*256}'
}

# --- build one arch -------------------------------------------------------
HOST_ARCH="$(uname -m)"
case "$HOST_ARCH" in
  x86_64|amd64) HOST_ARCH="amd64" ;;
  aarch64|arm64) HOST_ARCH="arm64" ;;
  *) HOST_ARCH="" ;;
esac

build_one() {
  arch="$1"
  case "$arch" in
    amd64) want=62 ;;
    arm64) want=183 ;;
    *) echo "ERROR: unsupported arch '$arch' (use amd64|arm64)" >&2; exit 1 ;;
  esac
  out="bin/$APP-linux-$arch"
  echo "==> building linux/$arch -> $out"
  mkdir -p bin checksums
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" "$GO" build \
    -trimpath -buildvcs=false \
    -ldflags "$LDFLAGS" \
    -o "$out" .
  chmod 0755 "$out"

  # final self-checks for this binary
  [ -f "$out" ] || { echo "ERROR: $out missing" >&2; exit 1; }
  [ -x "$out" ] || { echo "ERROR: $out not executable" >&2; exit 1; }
  magic="$(head -c4 "$out" | od -An -tx1 | tr -d ' \n')"
  [ "$magic" = "7f454c46" ] || { echo "ERROR: $out is not an ELF (magic $magic)" >&2; exit 1; }
  got="$(elf_machine "$out")"
  [ "$got" = "$want" ] || { echo "ERROR: $out arch e_machine=$got, want $want" >&2; exit 1; }
  # Only execute when the build host matches the target arch (cross-built
  # binaries cannot run natively; the e_machine check above already verified
  # the architecture).
  if [ "$HOST_ARCH" = "$arch" ]; then
    out_ver="$("$out" --version 2>&1 || true)"
    case "$out_ver" in
      "tiny-xboard version=$VERSION commit=$COMMIT built=$BUILD_TIME"*)
        : ;;
      *)
        echo "ERROR: $out --version check failed: $out_ver" >&2; exit 1 ;;
    esac
    echo "==> checked: $out --version ok (e_machine=$got)"
  else
    echo "==> checked: $out arch e_machine=$got (cross-built, exec check skipped)"
  fi
}

# --- run ------------------------------------------------------------------
arches=""
if [ "$#" -gt 0 ]; then
  case "$1" in
    amd64|arm64) arches="$1" ;;
    all) arches="amd64 arm64" ;;
    *) echo "ERROR: unknown target '$1' (use amd64|arm64|all)" >&2; exit 1 ;;
  esac
else
  arches="amd64 arm64"
fi

for a in $arches; do
  build_one "$a"
done

# --- aggregate SHA256SUMS -------------------------------------------------
echo "==> writing checksums/SHA256SUMS"
rm -f checksums/SHA256SUMS
for a in $arches; do
  sha256_ "bin/$APP-linux-$a" >> checksums/SHA256SUMS
done
sha256sum -c checksums/SHA256SUMS >/dev/null 2>&1 || {
  echo "ERROR: SHA256SUMS self-check failed" >&2; exit 1
}

# --- summary --------------------------------------------------------------
echo ""
echo "Build complete"
printf '%-12s %-10s %-10s %-12s %-10s %s\n' "Architecture" "Size" "SHA256" "Commit" "Build" "Binary"
for a in $arches; do
  out="bin/$APP-linux-$a"
  size="$(wc -c < "$out" | awk '{printf "%.1fMB", $1/1048576}')"
  hash="$(sha256_ "$out" | awk '{print $1}' | cut -c1-16)"
  printf '%-12s %-10s %-10s %-12s %-10s %s\n' "$a" "$size" "$hash" "$COMMIT" "$BUILD_TIME" "$out"
done
echo ""
echo "Artifacts:"
for a in $arches; do echo "  bin/$APP-linux-$a"; done
echo "  checksums/SHA256SUMS"
echo "Commit: $COMMIT"
echo "Build time: $BUILD_TIME"
echo "To publish: commit bin/ and checksums/ to the repository, then push."