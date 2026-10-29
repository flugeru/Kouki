// Command mkicon converts a square PNG into a valid multi-resolution Windows .ico.
//
// It is part of the normal build so the icon is reproducible (see build.ps1).
//
// Usage:
//
//	mkicon -in icon-512.png -out kouki.ico
//	mkicon -verify kouki.ico
//
// Resolutions emitted: 16, 24, 32, 48, 64, 128, 256.
//
// Entries smaller than 256px are stored as classic uncompressed DIBs (BITMAPINFOHEADER
// + XOR bitmap + AND mask) for maximum compatibility with every Windows surface
// (Explorer, taskbar, Start Menu, tray). The 256px entry is stored as PNG, which is the
// Vista+ convention and keeps the file small.
//
// Downscaling is a proper area-average (box filter) performed in *premultiplied alpha*
// space, so fully transparent pixels cannot bleed colour into visible edges and the
// artwork keeps its transparency with no unwanted background.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"sort"
)

type entry struct {
	w, h int
	data []byte
}

// ---------------------------------------------------------------------------
// scaling
// ---------------------------------------------------------------------------

// premul holds a premultiplied-alpha NRGBA buffer suitable for box-filtering.
type premul struct {
	w, h int       // premul.w == srcW, premul.h == srcH
	pix  []float64 // len = w*h*4, R,G,B,A all premultiplied, range 0..1
}

func toPremul(src *image.NRGBA) *premul {
	p := &premul{w: src.Bounds().Dx(), h: src.Bounds().Dy(), pix: make([]float64, src.Bounds().Dx()*src.Bounds().Dy()*4)}
	b := src.Bounds()
	i := 0
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			o := src.PixOffset(b.Min.X+x, b.Min.Y+y)
			r := float64(src.Pix[o+0]) / 255
			g := float64(src.Pix[o+1]) / 255
			bl := float64(src.Pix[o+2]) / 255
			a := float64(src.Pix[o+3]) / 255
			p.pix[i+0] = r * a
			p.pix[i+1] = g * a
			p.pix[i+2] = bl * a
			p.pix[i+3] = a
			i += 4
		}
	}
	return p
}

// resizeBox performs an area-average downscale to dw x dh, returning straight (non
// premultiplied) NRGBA. When upscaling is requested it falls back to nearest-neighbour.
func resizeBox(src *image.NRGBA, dw, dh int) *image.NRGBA {
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	if dw > sw || dh > sh {
		// nearest neighbour (should not happen with a 512px source)
		for y := 0; y < dh; y++ {
			for x := 0; x < dw; x++ {
				sx := x * sw / dw
				sy := y * sh / dh
				so := src.PixOffset(sx, sy)
				do := dst.PixOffset(x, y)
				copy(dst.Pix[do:do+4], src.Pix[so:so+4])
			}
		}
		return dst
	}

	p := toPremul(src)
	for dy := 0; dy < dh; dy++ {
		// source row range covered by this destination row
		y0 := dy * sh / dh
		y1 := (dy + 1) * sh / dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for dx := 0; dx < dw; dx++ {
			x0 := dx * sw / dw
			x1 := (dx + 1) * sw / dw
			if x1 <= x0 {
				x1 = x0 + 1
			}

			var sr, sg, sb, sa, n float64
			for sy := y0; sy < y1; sy++ {
				base := sy * sw * 4
				for sx := x0; sx < x1; sx++ {
					o := base + sx*4
					sr += p.pix[o+0]
					sg += p.pix[o+1]
					sb += p.pix[o+2]
					sa += p.pix[o+3]
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			sr /= n
			sg /= n
			sb /= n
			sa /= n

			do := dst.PixOffset(dx, dy)
			if sa <= 0 {
				dst.Pix[do+0], dst.Pix[do+1], dst.Pix[do+2], dst.Pix[do+3] = 0, 0, 0, 0
				continue
			}
			// unpremultiply
			dst.Pix[do+0] = clamp8(sr / sa)
			dst.Pix[do+1] = clamp8(sg / sa)
			dst.Pix[do+2] = clamp8(sb / sa)
			dst.Pix[do+3] = clamp8(sa)
		}
	}
	return dst
}

func clamp8(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 1 {
		return 255
	}
	return uint8(v*255 + 0.5)
}

// ---------------------------------------------------------------------------
// ICO encoding
// ---------------------------------------------------------------------------

// encodeDIB writes the classic ICO bitmap: BITMAPINFOHEADER (with doubled height),
// the bottom-up BGRA XOR bitmap, then the 1bpp bottom-up AND mask.
func encodeDIB(img *image.NRGBA) []byte {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()

	var b bytes.Buffer

	// BITMAPINFOHEADER
	bi := make([]byte, 40)
	binary.LittleEndian.PutUint32(bi[0:], 40)          // biSize
	binary.LittleEndian.PutUint32(bi[4:], uint32(w))   // biWidth
	binary.LittleEndian.PutUint32(bi[8:], uint32(2*h)) // biHeight = XOR + AND
	binary.LittleEndian.PutUint16(bi[12:], 1)          // biPlanes
	binary.LittleEndian.PutUint16(bi[14:], 32)         // biBitCount
	binary.LittleEndian.PutUint32(bi[16:], 0)          // biCompression = BI_RGB
	binary.LittleEndian.PutUint32(bi[20:], 0)          // biSizeImage (0 = allowed)
	binary.LittleEndian.PutUint32(bi[24:], 0)          // biXPelsPerMeter
	binary.LittleEndian.PutUint32(bi[28:], 0)          // biYPelsPerMeter
	binary.LittleEndian.PutUint32(bi[32:], 0)          // biClrUsed
	binary.LittleEndian.PutUint32(bi[36:], 0)          // biClrImportant
	b.Write(bi)

	// XOR bitmap: bottom-up, BGRA
	xor := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		srcY := h - 1 - y // DIB rows are stored bottom-up
		for x := 0; x < w; x++ {
			so := img.PixOffset(x, srcY)
			do := (y*w + x) * 4
			xor[do+0] = img.Pix[so+2] // B
			xor[do+1] = img.Pix[so+1] // G
			xor[do+2] = img.Pix[so+0] // R
			xor[do+3] = img.Pix[so+3] // A
		}
	}
	b.Write(xor)

	// AND mask: 1bpp, bottom-up, rows padded to 4 bytes.
	// Windows uses the alpha channel for 32bpp icons, but the mask must still exist.
	stride := ((w + 31) / 32) * 4
	and := make([]byte, stride*h)
	for y := 0; y < h; y++ {
		srcY := h - 1 - y
		for x := 0; x < w; x++ {
			a := img.Pix[img.PixOffset(x, srcY)+3]
			if a == 0 {
				and[y*stride+(x/8)] |= 0x80 >> uint(x%8) // 1 = transparent
			}
		}
	}
	b.Write(and)

	return b.Bytes()
}

func encodePNG(img *image.NRGBA) ([]byte, error) {
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildICO(entries []entry) []byte {
	sort.Slice(entries, func(i, j int) bool { return entries[i].w < entries[j].w })

	var out bytes.Buffer
	// ICONDIR
	out.Write([]byte{0, 0, 1, 0})
	_ = binary.Write(&out, binary.LittleEndian, uint16(len(entries)))

	offset := 6 + 16*len(entries)
	for _, e := range entries {
		w, h := e.w, e.h
		var wb, hb byte
		if w >= 256 {
			wb = 0
		} else {
			wb = byte(w)
		}
		if h >= 256 {
			hb = 0
		} else {
			hb = byte(h)
		}
		out.WriteByte(wb)
		out.WriteByte(hb)
		out.WriteByte(0)                                        // colour count (0 = truecolour)
		out.WriteByte(0)                                        // reserved
		_ = binary.Write(&out, binary.LittleEndian, uint16(1))  // planes
		_ = binary.Write(&out, binary.LittleEndian, uint16(32)) // bit count
		_ = binary.Write(&out, binary.LittleEndian, uint32(len(e.data)))
		_ = binary.Write(&out, binary.LittleEndian, uint32(offset))
		offset += len(e.data)
	}
	for _, e := range entries {
		out.Write(e.data)
	}
	return out.Bytes()
}

// ---------------------------------------------------------------------------
// verification
// ---------------------------------------------------------------------------

func verify(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(raw) < 6 {
		return fmt.Errorf("too small to be an ICO")
	}
	if binary.LittleEndian.Uint16(raw[0:2]) != 0 {
		return fmt.Errorf("bad reserved field")
	}
	if typ := binary.LittleEndian.Uint16(raw[2:4]); typ != 1 {
		return fmt.Errorf("bad type field %d (want 1 = icon)", typ)
	}
	n := int(binary.LittleEndian.Uint16(raw[4:6]))
	fmt.Printf("%s: %d entries, %d bytes\n", path, n, len(raw))
	ok := true
	for i := 0; i < n; i++ {
		off := 6 + 16*i
		if off+16 > len(raw) {
			return fmt.Errorf("directory truncated at entry %d", i)
		}
		w := int(raw[off])
		h := int(raw[off+1])
		if w == 0 {
			w = 256
		}
		if h == 0 {
			h = 256
		}
		planes := binary.LittleEndian.Uint16(raw[off+4 : off+6])
		bits := binary.LittleEndian.Uint16(raw[off+6 : off+8])
		size := binary.LittleEndian.Uint32(raw[off+8 : off+12])
		imgOff := binary.LittleEndian.Uint32(raw[off+12 : off+16])

		end := uint64(imgOff) + uint64(size)
		if end > uint64(len(raw)) {
			fmt.Printf("  entry %d: %dx%d  *** DATA OUT OF BOUNDS ***\n", i, w, h)
			ok = false
			continue
		}
		kind := "DIB"
		if size >= 8 && raw[imgOff] == 0x89 && raw[imgOff+1] == 'P' && raw[imgOff+2] == 'N' && raw[imgOff+3] == 'G' {
			kind = "PNG"
		}
		fmt.Printf("  entry %d: %3dx%-3d  %4s  %6d bytes  planes=%d bits=%d\n",
			i, w, h, kind, size, planes, bits)
	}
	if !ok {
		return fmt.Errorf("one or more entries are corrupt")
	}
	return nil
}

// ---------------------------------------------------------------------------

func main() {
	in := flag.String("in", "", "source PNG (square)")
	out := flag.String("out", "", "destination .ico")
	verifyPath := flag.String("verify", "", "just verify an existing .ico and exit")
	flag.Parse()

	if *verifyPath != "" {
		if err := verify(*verifyPath); err != nil {
			fmt.Fprintln(os.Stderr, "VERIFY FAILED:", err)
			os.Exit(1)
		}
		fmt.Println("ICO OK")
		return
	}
	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: mkicon -in icon.png -out icon.ico   |   mkicon -verify icon.ico")
		os.Exit(2)
	}

	f, err := os.Open(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open input:", err)
		os.Exit(1)
	}
	srcImg, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "decode input:", err)
		os.Exit(1)
	}

	b := srcImg.Bounds()
	sw, sh := b.Dx(), b.Dy()
	fmt.Printf("source: %dx%d\n", sw, sh)
	if sw != sh {
		fmt.Fprintf(os.Stderr, "warning: source is not square (%dx%d); using a centred square crop to avoid distortion\n", sw, sh)
	}
	side := sw
	if sh < side {
		side = sh
	}
	// centred square crop (no-op for square sources)
	x0 := b.Min.X + (sw-side)/2
	y0 := b.Min.Y + (sh-side)/2
	crop := image.NewNRGBA(image.Rect(0, 0, side, side))
	draw.Draw(crop, crop.Bounds(), srcImg, image.Point{x0, y0}, draw.Src)

	// Normalise to NRGBA regardless of what the decoder produced.
	nrgba := image.NewNRGBA(crop.Bounds())
	draw.Draw(nrgba, nrgba.Bounds(), crop, crop.Bounds().Min, draw.Src)

	sizes := []int{16, 24, 32, 48, 64, 128, 256}
	var entries []entry
	for _, s := range sizes {
		img := resizeBox(nrgba, s, s)
		var data []byte
		if s >= 256 {
			data, err = encodePNG(img)
			if err != nil {
				fmt.Fprintln(os.Stderr, "encode png:", err)
				os.Exit(1)
			}
		} else {
			data = encodeDIB(img)
		}
		entries = append(entries, entry{w: s, h: s, data: data})
		fmt.Printf("  %3dx%-3d  %6d bytes\n", s, s, len(data))
	}

	ico := buildICO(entries)
	if err := os.WriteFile(*out, ico, 0644); err != nil {
		fmt.Fprintln(os.Stderr, "write output:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes)\n", *out, len(ico))

	if err := verify(*out); err != nil {
		fmt.Fprintln(os.Stderr, "post-write verify FAILED:", err)
		os.Exit(1)
	}
	fmt.Println("ICO OK")
}
