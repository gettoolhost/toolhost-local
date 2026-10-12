#!/usr/bin/env bash
set -euo pipefail

# install.sh — one-line installer for toolhost.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/gettoolhost/toolhost-local/main/install.sh | bash
#   curl -fsSL ... | bash -s -- --dir /usr/local/bin   # custom install directory
#
# Environment:
#   TOOLHOST_DOWNLOAD_URL  Override the release-asset base URL (for testing)

# Wrap in main() so a truncated pipe can never execute a partial script —
# main is only invoked by the last line, which either arrived or didn't.
main() {

REPO="gettoolhost/toolhost-local"
INSTALL_DIR="$HOME/.local/bin"
TOOLHOST_DOWNLOAD_URL="${TOOLHOST_DOWNLOAD_URL:-https://github.com/${REPO}/releases/latest/download}"

# Every remote hop stays HTTPS. Plain HTTP is accepted only for an exact
# loopback authority used by local smoke tests, with redirects disabled so
# a fixture cannot bounce the installer to the network.
is_loopback_http_url() {
    [[ "$1" =~ ^http://(localhost|127\.0\.0\.1|\[::1\])(:[0-9]+)?([/?\#].*)?$ ]]
}

if [[ "$TOOLHOST_DOWNLOAD_URL" == https://* ]]; then
    LOOPBACK=false
elif is_loopback_http_url "$TOOLHOST_DOWNLOAD_URL"; then
    LOOPBACK=true
else
    echo "error: refusing non-HTTPS download URL: $TOOLHOST_DOWNLOAD_URL" >&2
    exit 1
fi

download_file() {
    local url="$1" dest="$2"
    if [ "$LOOPBACK" = true ]; then
        is_loopback_http_url "$url" || { echo "error: loopback download escaped its authority: $url" >&2; return 1; }
        curl -fS --noproxy '*' --proto '=http' -o "$dest" "$url"
        return
    fi
    [[ "$url" == https://* ]] || { echo "error: HTTPS download downgraded: $url" >&2; return 1; }
    curl -fSL --max-redirs 5 --proto '=https' --proto-redir '=https' -o "$dest" "$url"
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --dir=*) INSTALL_DIR="${1#--dir=}"; shift ;;
        --dir)
            if [ "$#" -lt 2 ] || [[ "$2" == -* ]]; then
                echo "install.sh: '--dir' needs a value" >&2; exit 2
            fi
            INSTALL_DIR="$2"; shift 2 ;;
        -h|--help)
            grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) echo "install.sh: unknown option $1" >&2; exit 2 ;;
    esac
done

OS="$(uname -s)"
ARCH="$(uname -m)"
case "$OS" in
    Darwin) os=darwin ;;
    Linux)  os=linux ;;
    *)
        echo "error: unsupported OS $OS — download the Windows zip from" >&2
        echo "  https://github.com/${REPO}/releases/latest" >&2
        exit 1 ;;
esac
case "$ARCH" in
    arm64|aarch64) arch=arm64 ;;
    x86_64|amd64)  arch=amd64 ;;
    *) echo "error: unsupported architecture $ARCH" >&2; exit 1 ;;
esac

archive="toolhost_${os}_${arch}.tar.gz"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "downloading toolhost (${os}/${arch})..."
download_file "${TOOLHOST_DOWNLOAD_URL}/${archive}" "${tmp}/${archive}"
download_file "${TOOLHOST_DOWNLOAD_URL}/checksums.txt" "${tmp}/checksums.txt"

# Verify the archive against the release checksums before it runs
# anywhere. cosign attestations cover the checksums file itself; this
# check is the download-integrity floor every install gets.
if command -v shasum &>/dev/null; then
    SUM="shasum -a 256"
elif command -v sha256sum &>/dev/null; then
    SUM="sha256sum"
else
    echo "error: shasum or sha256sum required to verify the download" >&2
    exit 1
fi
want="$(awk -v a="$archive" '$2 == a {print $1}' "${tmp}/checksums.txt")"
[ -n "$want" ] || { echo "error: ${archive} not found in checksums.txt" >&2; exit 1; }
got="$($SUM "${tmp}/${archive}" | awk '{print $1}')"
if [ "$want" != "$got" ]; then
    echo "error: checksum mismatch for ${archive}" >&2
    echo "  want: $want" >&2
    echo "  got:  $got" >&2
    exit 1
fi

tar xzf "${tmp}/${archive}" -C "$tmp"
mkdir -p "$INSTALL_DIR"
mv "${tmp}/toolhost" "${INSTALL_DIR}/toolhost"
chmod 755 "${INSTALL_DIR}/toolhost"

if [ "$os" = darwin ]; then
    # Downloaded executables arrive quarantined; strip it and ad-hoc sign
    # so Gatekeeper and Gatekeeper-adjacent checks accept the binary.
    xattr -d com.apple.quarantine "${INSTALL_DIR}/toolhost" 2>/dev/null || true
    codesign -s - --force "${INSTALL_DIR}/toolhost" 2>/dev/null || true
fi

echo "installed ${INSTALL_DIR}/toolhost"
case ":$PATH:" in
    *":${INSTALL_DIR}:"*) ;;
    *) echo "note: ${INSTALL_DIR} is not on your PATH" >&2 ;;
esac
echo ""
echo "next: toolhost init && toolhost doctor && toolhost attach"

}

main "$@"
