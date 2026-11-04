#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
command -v appimagetool >/dev/null || { echo 'Instale appimagetool e coloque no PATH.' >&2; exit 1; }
cargo build --locked --release
version="$(sed -n 's/^version = "\([^"]*\)"/\1/p' Cargo.toml | head -n1)"
build="$(mktemp -d)"
trap 'rm -rf -- "$build"' EXIT
app="$build/Kouki.AppDir"
mkdir -p "$app/usr/bin" dist
cp target/release/kouki "$app/usr/bin/kouki"
cp assets/icon/icon-512.png "$app/kouki.png"
cat > "$app/AppRun" <<'EOF'
#!/bin/sh
set -eu
APPDIR="${APPDIR:-$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)}"
exec "$APPDIR/usr/bin/kouki" "$@"
EOF
chmod +x "$app/AppRun"
cat > "$app/kouki.desktop" <<'EOF'
[Desktop Entry]
Type=Application
Name=Kouki
Exec=kouki
Icon=kouki
Categories=AudioVideo;Player;
Terminal=false
EOF
# ponytail: system graphics libraries; test target distro before distributing.
ARCH="$(uname -m)" APPIMAGE_EXTRACT_AND_RUN=1 appimagetool "$app" "dist/Kouki-Rust-$version-$(uname -m).AppImage"
