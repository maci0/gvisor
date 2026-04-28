#!/bin/bash
# macsc — run Linux containers on macOS using gVisor
#
# Usage:
#   macsc run --rootfs <dir> [--strace] -- <command> [args...]
#   macsc run --rootfs <dir> -- /bin/sh        # interactive shell
#   macsc build                                 # build sentrydarwin
#
# Examples:
#   macsc run --rootfs ./alpine-rootfs -- /bin/sh -c 'ls /'
#   macsc run --rootfs ./alpine-rootfs -- /bin/sh -c 'cat /etc/os-release'

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BAZEL_BINARY="$REPO_ROOT/bazel-bin/cmd/sentrydarwin/sentrydarwin_/sentrydarwin"
BINARY="$REPO_ROOT/_tmp/sentrydarwin"
ENTITLEMENTS="$REPO_ROOT/_tmp/entitlements.plist"

# Create entitlements file if missing
ensure_entitlements() {
    if [ ! -f "$ENTITLEMENTS" ]; then
        mkdir -p "$(dirname "$ENTITLEMENTS")"
        cat > "$ENTITLEMENTS" << 'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>com.apple.security.hypervisor</key>
    <true/>
</dict>
</plist>
PLIST
    fi
}

cmd_build() {
    echo "Building sentrydarwin..."
    cd "$REPO_ROOT"
    bazel build --config=hvf //cmd/sentrydarwin:sentrydarwin
    mkdir -p "$(dirname "$BINARY")"
    rm -f "$BINARY"
    cp "$BAZEL_BINARY" "$BINARY"
    ensure_entitlements
    codesign --force --sign - --entitlements "$ENTITLEMENTS" "$BINARY"
    echo "Build complete: $BINARY"
}

cmd_run() {
    if [ ! -f "$BINARY" ]; then
        echo "Binary not found. Run 'macsc build' first."
        exit 1
    fi

    # Check if binary needs re-signing (e.g., after rebuild)
    if ! codesign -v "$BINARY" 2>/dev/null; then
        ensure_entitlements
        codesign --force --sign - --entitlements "$ENTITLEMENTS" "$BINARY"
    fi

    exec "$BINARY" "$@"
}

cmd_pull() {
    local image="${1:-alpine}"
    local dest="${2:-$REPO_ROOT/_tmp/${image}-rootfs}"

    case "$image" in
        alpine)
            local version="3.21"
            local release="3.21.3"
            local url="https://dl-cdn.alpinelinux.org/alpine/v${version}/releases/aarch64/alpine-minirootfs-${release}-aarch64.tar.gz"
            ;;
        *)
            echo "Unknown image: $image (supported: alpine)"
            exit 1
            ;;
    esac

    if [ -d "$dest" ]; then
        echo "Rootfs already exists: $dest"
        return
    fi

    echo "Downloading $image rootfs..."
    mkdir -p "$dest"
    curl -L "$url" | tar -xz -C "$dest"
    echo "Rootfs ready: $dest"
}

case "${1:-help}" in
    build)
        cmd_build
        ;;
    run)
        shift
        cmd_run "$@"
        ;;
    pull)
        shift
        cmd_pull "$@"
        ;;
    help|--help|-h)
        echo "macsc — run Linux containers on macOS using gVisor"
        echo ""
        echo "Usage:"
        echo "  macsc build                                Build the sentry binary"
        echo "  macsc pull [alpine]                        Download a rootfs image"
        echo "  macsc run --rootfs <dir> <cmd> [args]      Run a command"
        echo "  macsc run --rootfs <dir> --strace <cmd>    Run with syscall tracing"
        echo ""
        echo "Quick start:"
        echo "  macsc build"
        echo "  macsc pull alpine"
        echo "  macsc run --rootfs _tmp/alpine-rootfs /bin/sh -c 'ls /'"
        ;;
    *)
        echo "Unknown command: $1"
        echo "Run 'macsc help' for usage."
        exit 1
        ;;
esac
