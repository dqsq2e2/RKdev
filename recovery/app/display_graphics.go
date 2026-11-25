package main

import (
	"compress/gzip"
	_ "embed"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"os"
	"strings"
	"syscall"
	"unicode"
	"unsafe"
)

const setupWidth, setupHeight = 1024, 576
const (
	fbioVar    = 0x4600
	fbioFix    = 0x4602
	fbioBlank  = 0x4611
	kdMode     = 0x4b3a
	kdText     = 0
	kdGraphics = 1
	vtState    = 0x5603
)

type fbField struct{ offset, bits, reversed uint32 }
type fbVar struct {
	w, h, vw, vh, x, y, bpp, gray                      uint32
	red, green, blue, alpha                            fbField
	nonstd, activate, height, width, accel, pixelclock uint32
	timing                                             [10]uint32
	reserved                                           [4]uint32
}
type fbFix struct {
	id                    [16]byte
	smem                  uintptr
	len, typ, aux, visual uint32
	xpan, ypan, ywrap     uint16
	line                  uint32
	mmio                  uintptr
	mmioLen, accel        uint32
	caps                  uint16
	reserved              [2]uint16
}

type framebuffer struct {
	file *os.File
	mem  []byte
	v    fbVar
	f    fbFix
}

func devIO(f *os.File, ctl, arg uintptr) error {
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), ctl, arg)
	if err != 0 {
		return err
	}
	return nil
}

func devIOPtr(f *os.File, ctl uintptr, arg unsafe.Pointer) error {
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), ctl, uintptr(arg))
	if err != 0 {
		return err
	}
	return nil
}

func openFramebuffer() (*framebuffer, error) {
	file, err := os.OpenFile("/dev/fb0", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	frame := &framebuffer{file: file}
	if err := frame.refresh(); err != nil {
		frame.close()
		return nil, err
	}
	return frame, nil
}

func (f *framebuffer) refresh() error {
	var v fbVar
	var fixed fbFix
	if err := devIOPtr(f.file, fbioVar, unsafe.Pointer(&v)); err != nil {
		return err
	}
	if err := devIOPtr(f.file, fbioFix, unsafe.Pointer(&fixed)); err != nil {
		return err
	}
	if f.mem != nil && v == f.v && fixed == f.f {
		return nil
	}
	if fixed.typ != 0 || fixed.visual != 2 || v.nonstd != 0 || v.gray != 0 ||
		(v.bpp != 16 && v.bpp != 24 && v.bpp != 32) ||
		v.w < 640 || v.h < 360 || v.w > 8192 || v.h > 8192 ||
		fixed.len == 0 || fixed.len > 256<<20 ||
		uint64(fixed.line)*(uint64(v.y)+uint64(v.h)) > uint64(fixed.len) ||
		(uint64(v.x)+uint64(v.w))*uint64(v.bpp/8) > uint64(fixed.line) {
		return fmt.Errorf("当前显示模式不支持本地界面")
	}
	for _, field := range []fbField{v.red, v.green, v.blue, v.alpha} {
		if field.reversed != 0 || field.bits > 8 || field.offset+field.bits > v.bpp {
			return fmt.Errorf("不支持的帧缓冲像素格式")
		}
	}
	mem, err := syscall.Mmap(int(f.file.Fd()), 0, int(fixed.len), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return err
	}
	if f.mem != nil {
		_ = syscall.Munmap(f.mem)
	}
	f.mem, f.v, f.f = mem, v, fixed
	return nil
}

func (f *framebuffer) close() {
	if f.mem != nil {
		_ = syscall.Munmap(f.mem)
	}
	_ = f.file.Close()
}

func (f *framebuffer) paint(src *image.RGBA) error {
	v := f.v
	pixelSize := int(v.bpp / 8)
	screenW, screenH := int(v.w), int(v.h)
	w, h := screenW, screenW*setupHeight/setupWidth
	if h > screenH {
		w, h = screenH*setupWidth/setupHeight, screenH
	}
	left, top := (screenW-w)/2, (screenH-h)/2
	xs := make([]int, w)
	for i := range xs {
		xs[i] = 4 * (i * setupWidth / w)
	}
	stride := int(f.f.line)
	off := int(v.y)*stride + int(v.x)*pixelSize
	alpha := ((uint32(1) << v.alpha.bits) - 1) << v.alpha.offset
	xrgb := v.red == (fbField{16, 8, 0}) && v.green == (fbField{8, 8, 0}) && v.blue == (fbField{0, 8, 0}) && pixelSize == 4
	component := func(c uint8, b fbField) uint32 {
		return (uint32(c) * ((1 << b.bits) - 1) / 255) << b.offset
	}
	for y := range screenH {
		row := f.mem[off+y*stride : off+y*stride+screenW*pixelSize]
		if y < top || y >= top+h {
			clear(row)
			continue
		}
		clear(row[:left*pixelSize])
		clear(row[(left+w)*pixelSize:])
		in := src.Pix[(y-top)*setupHeight/h*src.Stride:]
		if xrgb {
			for i, x := range xs {
				j := (i + left) * 4
				row[j], row[j+1], row[j+2], row[j+3] = in[x+2], in[x+1], in[x], byte(alpha>>24)
			}
			continue
		}
		for i, x := range xs {
			p := component(in[x], v.red) | component(in[x+1], v.green) | component(in[x+2], v.blue) | alpha
			j := (i + left) * pixelSize
			for b := range pixelSize {
				row[j+b] = byte(p >> (8 * b))
			}
		}
	}
	// Pixel writes alone do not restore an output blanked during console handoff.
	return devIO(f.file, fbioBlank, 0)
}

type setupTTY struct {
	file  *os.File
	state syscall.Termios
}

func openSetupTTY() (*setupTTY, error) {
	f, err := os.OpenFile("/dev/tty1", os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	t := &setupTTY{file: f}
	if err := devIOPtr(f, syscall.TCGETS, unsafe.Pointer(&t.state)); err != nil {
		f.Close()
		return nil, err
	}
	if err := t.graphics(); err != nil {
		f.Close()
		return nil, err
	}
	return t, nil
}

func (t *setupTTY) graphics() error {
	raw := t.state
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN | syscall.ISIG
	raw.Iflag &^= syscall.IXON | syscall.ICRNL
	if err := devIOPtr(t.file, syscall.TCSETS, unsafe.Pointer(&raw)); err != nil {
		return err
	}
	if err := devIO(t.file, kdMode, kdGraphics); err != nil {
		t.text()
		return err
	}
	return nil
}

func (t *setupTTY) text() {
	_ = devIO(t.file, kdMode, kdText)
	_ = devIOPtr(t.file, syscall.TCSETS, unsafe.Pointer(&t.state))
}

func (t *setupTTY) active() bool {
	var status [3]uint16
	return devIOPtr(t.file, vtState, unsafe.Pointer(&status)) == nil && status[0] == 1
}

//go:embed display/setup-font.gz
var setupFontData string

type setupGlyph struct {
	width  int
	bitmap []byte
}

type setupPainter struct {
	image  *image.RGBA
	glyphs map[rune]setupGlyph
	logo   image.Image
}

func newSetupPainter() (*setupPainter, error) {
	f, err := gzip.NewReader(strings.NewReader(setupFontData))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	p := &setupPainter{
		image:  image.NewRGBA(image.Rect(0, 0, setupWidth, setupHeight)),
		glyphs: map[rune]setupGlyph{},
	}
	for len(raw) > 0 {
		if len(raw) < 5 {
			return nil, fmt.Errorf("invalid setup font")
		}
		ch, w := rune(binary.LittleEndian.Uint32(raw)), int(raw[4])
		size := 5 + w*23
		if w < 3 || w > 22 || size > len(raw) {
			return nil, fmt.Errorf("invalid setup glyph")
		}
		p.glyphs[ch] = setupGlyph{w, raw[5:size]}
		raw = raw[size:]
	}
	if img, err := resFS.Open("res/boot-mark.png"); err == nil {
		p.logo, _ = png.Decode(img)
		img.Close()
	}
	return p, nil
}

func setupColor(rgb uint32) color.RGBA {
	return color.RGBA{uint8(rgb >> 16), uint8(rgb >> 8), uint8(rgb), 255}
}

func (p *setupPainter) box(x, y, w, h int, rgb uint32) {
	draw.Draw(p.image, image.Rect(x, y, x+w, y+h), image.NewUniform(setupColor(rgb)), image.Point{}, draw.Src)
}

func (p *setupPainter) width(s string) int {
	w := 0
	for _, ch := range s {
		g, ok := p.glyphs[ch]
		if !ok {
			g = p.glyphs['?']
		}
		w += g.width
	}
	return w
}

func (p *setupPainter) wrap(x, y, limit, lines int, s string, rgb uint32) {
	for line := 0; line < lines && s != ""; line++ {
		n, w := 0, 0
		for i, ch := range s {
			w += p.width(string(ch))
			if w > limit-x || ch == '\n' {
				n = i
				break
			}
			n = len(s)
		}
		if n == 0 {
			break
		}
		p.text(x, y+line*24, limit, s[:n], rgb)
		s = strings.TrimLeft(s[n:], " \n")
	}
}

func (p *setupPainter) text(x, y, limit int, s string, rgb uint32) {
	c := setupColor(rgb)
	for _, ch := range s {
		if unicode.IsControl(ch) {
			ch = ' '
		}
		g, ok := p.glyphs[ch]
		if !ok {
			g = p.glyphs['?']
		}
		if x+g.width > limit {
			break
		}
		for gy := range 23 {
			if y+gy < 0 || y+gy >= setupHeight {
				continue
			}
			px := p.image.Pix[(y+gy)*p.image.Stride+x*4:]
			src := g.bitmap[gy*g.width:]
			for gx := range g.width {
				a := int(src[gx])
				if a == 0 {
					continue
				}
				i := 4 * gx
				px[i] = byte((int(px[i])*(255-a) + int(c.R)*a) / 255)
				px[i+1] = byte((int(px[i+1])*(255-a) + int(c.G)*a) / 255)
				px[i+2] = byte((int(px[i+2])*(255-a) + int(c.B)*a) / 255)
			}
		}
		x += g.width
	}
}

func (p *setupPainter) mark() {
	if p.logo == nil {
		return
	}
	b := p.logo.Bounds()
	const dstH = 36
	dstW := b.Dx() * dstH / b.Dy()
	dstW = min(dstW, 84)
	for y := range dstH {
		for x := range dstW {
			src := color.NRGBAModel.Convert(p.logo.At(b.Min.X+x*b.Dx()/dstW, b.Min.Y+y*b.Dy()/dstH)).(color.NRGBA)
			if src.A == 0 {
				continue
			}
			bg := p.image.RGBAAt(26+x, 15+y)
			a := int(src.A)
			p.image.SetRGBA(26+x, 15+y, color.RGBA{
				uint8((a*int(src.R) + (255-a)*int(bg.R)) / 255),
				uint8((a*int(src.G) + (255-a)*int(bg.G)) / 255),
				uint8((a*int(src.B) + (255-a)*int(bg.B)) / 255), 255})
		}
	}
}
