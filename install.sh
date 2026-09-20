#!/bin/sh
set -eu

main() {
    case "${SHELL:-}" in
        bash|zsh|*/bash|*/zsh) ;;
        *) echo "Unsupported shell: ${SHELL:-unset}. Only Bash and Zsh are supported." >&2; exit 1 ;;
    esac
    case "$(uname -s)" in
        Linux) os=linux ;;
        Darwin) os=darwin ;;
        *) echo 'Unsupported OS' >&2; exit 1 ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64) arch=amd64 ;;
        arm64|aarch64) arch=arm64 ;;
        *) echo 'Unsupported architecture' >&2; exit 1 ;;
    esac

    url="https://github.com/TheR1D/aty/releases/latest/download/aty_${os}_${arch}"
    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT
    trap 'exit 1' HUP INT TERM
    curl -fsSL "$url" -o "$tmp/aty"
    [ -s "$tmp/aty" ] || { echo 'Downloaded binary is empty' >&2; exit 1; }

    install_dir="$HOME/.local/bin"
    mkdir -p "$install_dir"
    install -m 0755 "$tmp/aty" "$install_dir/aty"
    echo 'Installed ATY binary into ~/.local/bin.'
    case ":${PATH:-}:" in
        *:"$install_dir":*) ;;
        *) echo 'Warning: ~/.local/bin is not in your PATH. Launch ATY using: ~/.local/bin/aty' >&2 ;;
    esac
}

main
