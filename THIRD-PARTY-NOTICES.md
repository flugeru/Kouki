# Third-Party Notices - Kouki 1.0.2

Kouki redistributes the following third-party software. Kouki itself is released
under the MIT license; the components below remain under their own licenses.

---

## mpv

The bundled media engine is **mpv**.

- Upstream: <https://mpv.io/> · <https://github.com/mpv-player/mpv>
- This build: `mpv v0.41.0-1122-gaccb7b282` (windows-build "shinchiro" flavour,
  x86_64), built 2026-10-11.
- License: **GPL v2 or later** (mpv is GPL-2.0+; it is not LGPL).

mpv is distributed as a **separate executable**. Kouki does not link against it and
does not embed `libmpv`: it launches the bundled `mpv.exe` and communicates with
it at runtime over the mpv JSON IPC channel (`\\.\pipe\mpv-pipe`). Kouki is
therefore not a derivative work of mpv for licensing purposes. mpv's full source
and license are available at the upstream links above.

mpv bundles or links the following, each under its own license:

| Component | License |
|---|---|
| FFmpeg (libav\*) | LGPL v2.1+ / GPL v2+ |
| libplacebo | LGPL v2.1+ |
| libass | ISC |
| LuaJIT | MIT |
| OpenAL Soft | LGPL v2.1+ |
| libjpeg-turbo | BSD-3-Clause / IJG |
| libvpx, dav1d, x264, x265, opus, vorbis, theora, fribidi, freetype, harfbuzz, libxml2, zlib, rubberband, libcurl, lcms2 | respective upstream licenses (mostly BSD/MIT/Apache-2.0/zlib) |

## uosc (mpv On-Screen Controller)

Bundled at `mpv/portable_config/scripts/uosc/`.

- Upstream: <https://github.com/uosc/uosc>
- License: **GPL v3 or later**

Also bundled from uosc:
- `bin/ziggy-windows.exe` — Ziggy (WASM-based JavaScript engine), Apache-2.0.
- `fonts/uosc_icons.otf`, `fonts/uosc_textures.ttf` — SIL Open Font License 1.1.

## thumbfast

Bundled at `mpv/portable_config/scripts/thumbfast.lua`.

- Upstream: <https://github.com/diegopolido/mpv-thumbfast>
- License: **MIT**

## SmartCopyPaste

Bundled at `mpv/portable_config/scripts/SmartCopyPaste.lua`.

- Upstream: <https://github.com/Eiderblank/...> (community script)
- License: as distributed by its author — see the script header.

## Video shaders

Bundled at `mpv/portable_config/shaders/`:

- `nnedi3-nns128-win8x4.hook` — NVIDIA NNEDI3 upscaler.
- `ArtCNN_C4F32.glsl` — ArtCNN (Anime super-resolution).

These are configuration/script assets, not compiled libraries; they carry no
warranty and are provided as-is.

## WebTorrent support

`portable_config/scripts/webtorrent.js` + `script-opts/webtorrent.conf` come from
the mpv "webtorrent" hook and are included unchanged from the user's original
configuration.

## Go dependencies

Kouki's own components are built with Go modules including:

| Module | License | Role |
|---|---|---|
| `github.com/lxn/walk` (+ `github.com/lxn/win`) | MIT | Main window (native Win32, pure Go) |
| `github.com/Microsoft/go-winio` | MIT | Windows named pipes (mpv + Discord IPC) |
| `github.com/5rahim/habari` | MIT | Anime/movie filename parser |
| `github.com/getlantern/systray` (+ context, errors, golog, hex, hidden, ops) | Apache-2.0 / MIT | Notification-area icon and menu |
| `github.com/huin/goupnp` | BSD-3-Clause | UPnP port mapping |
| `github.com/jackpal/go-nat-pmp` | MIT | NAT-PMP port mapping |
| `github.com/samber/lo` | MIT | Generic helpers (transitive) |
| `github.com/google/uuid` | BSD-3-Clause | Transitive (systray) |
| `golang.org/x/sys`, `golang.org/x/sync`, `golang.org/x/text` | BSD-3-Clause | Windows system calls and text |

Run `go mod download` in the source tree for the exact module graph.

## Build-time tools (not redistributed)

| Tool | License | Role |
|---|---|---|
| [Inno Setup 6](https://jrsoftware.org/isinfo.php) | see `LICENSE.TXT` upstream (commercial use permitted) | Compiles `installer/kouki.iss` into the setup executable |
| [goversioninfo](https://github.com/josephspurrier/goversioninfo) | MIT | Generates `kouki.syso` (icon + version + manifest) |
