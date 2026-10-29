#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$repo_root"

if [[ "$(uname -s)" != Linux ]]; then
  echo "O AppImage deve ser compilado no Linux." >&2
  exit 1
fi

case "$(uname -m)" in
  x86_64|aarch64) arch="$(uname -m)" ;;
  *) echo "Arquitetura Linux ainda não suportada: $(uname -m)" >&2; exit 1 ;;
esac

version="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || printf 'dev')}"
version="${version#v}"
version="${version//[^[:alnum:]._-]/-}"
output_dir="${OUTPUT_DIR:-$repo_root/dist}"
mkdir -p "$output_dir"
output_dir="$(cd "$output_dir" && pwd -P)"

build_dir="$(mktemp -d)"
trap 'rm -rf -- "$build_dir"' EXIT
app_dir="$build_dir/Kouki.AppDir"
mkdir -p "$app_dir/usr/bin" \
  "$app_dir/usr/share/applications" \
  "$app_dir/usr/share/icons/hicolor/512x512/apps" \
  "$app_dir/usr/share/doc/kouki"

CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' -o "$app_dir/usr/bin/kouki" .

cat > "$app_dir/AppRun" <<'EOF'
#!/bin/sh
set -eu
APPDIR="${APPDIR:-$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)}"
exec "$APPDIR/usr/bin/kouki" "$@"
EOF
chmod +x "$app_dir/AppRun"

cat > "$app_dir/kouki.desktop" <<'EOF'
[Desktop Entry]
Type=Application
Name=Kouki
Comment=Acompanhe animes e sincronize o progresso
Exec=kouki
Icon=kouki
Terminal=false
Categories=AudioVideo;Video;Player;
StartupNotify=true
EOF
cp "$app_dir/kouki.desktop" "$app_dir/usr/share/applications/kouki.desktop"
cp assets/icon/icon-512.png "$app_dir/kouki.png"
cp "$app_dir/kouki.png" "$app_dir/usr/share/icons/hicolor/512x512/apps/kouki.png"
ln -s kouki.png "$app_dir/.DirIcon"
cp LICENSE THIRD-PARTY-NOTICES.md README-Linux.md "$app_dir/usr/share/doc/kouki/"

if command -v desktop-file-validate >/dev/null 2>&1; then
  desktop-file-validate "$app_dir/kouki.desktop"
fi

if [[ -n "${APPIMAGETOOL:-}" ]]; then
  appimagetool="$APPIMAGETOOL"
elif command -v appimagetool >/dev/null 2>&1; then
  appimagetool="$(command -v appimagetool)"
else
  appimagetool="$build_dir/appimagetool-$arch.AppImage"
  curl --fail --location --silent --show-error \
    "https://github.com/AppImage/appimagetool/releases/download/1.9.1/appimagetool-$arch.AppImage" \
    --output "$appimagetool"
  chmod +x "$appimagetool"
fi

if [[ ! -x "$appimagetool" ]]; then
  echo "appimagetool não está executável: $appimagetool" >&2
  exit 1
fi

output="$output_dir/Kouki-$version-$arch.AppImage"
ARCH="$arch" APPIMAGE_EXTRACT_AND_RUN=1 "$appimagetool" "$app_dir" "$output"
chmod +x "$output"
printf 'AppImage criado: %s\n' "$output"
