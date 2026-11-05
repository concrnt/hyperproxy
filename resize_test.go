package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
)

var fixtures string

func TestMain(m *testing.M) {
	init_resize()

	dir, err := os.MkdirTemp("", "hyperproxy-test")
	if err != nil {
		panic(err)
	}
	fixtures = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func photo(w, h int, seed int64) *image.RGBA {
	r := rand.New(rand.NewSource(seed))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			n := uint8(r.Intn(32))
			img.Pix[img.PixOffset(x, y)+0] = uint8(x*255/w) ^ n
			img.Pix[img.PixOffset(x, y)+1] = uint8(y*255/h) ^ n
			img.Pix[img.PixOffset(x, y)+2] = uint8((x+y)*255/(w+h)) ^ n
			img.Pix[img.PixOffset(x, y)+3] = 255
		}
	}
	return img
}

// exifOrientation builds a minimal APP1 Exif segment carrying only the Orientation tag.
func exifOrientation(o uint16) []byte {
	tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 0x01, 3, 0, 1, 0, 0, 0, byte(o), byte(o >> 8), 0, 0, 0, 0, 0, 0}
	payload := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	return append(seg, payload...)
}

func fixture(t testing.TB, name string, gen func(path string) error) string {
	t.Helper()
	path := filepath.Join(fixtures, name)
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if err := gen(path); err != nil {
		t.Fatalf("generate %s: %v", name, err)
	}
	return path
}

func writeJPEG(w, h int, orientation uint16) func(string) error {
	return func(path string) error {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, photo(w, h, 1), &jpeg.Options{Quality: 90}); err != nil {
			return err
		}
		data := buf.Bytes()
		if orientation != 0 {
			data = append(append(append([]byte{}, data[:2]...), exifOrientation(orientation)...), data[2:]...)
		}
		return os.WriteFile(path, data, 0644)
	}
}

func writePNG(w, h int) func(string) error {
	return func(path string) error {
		img := photo(w, h, 2)
		for i := 3; i < len(img.Pix); i += 4 {
			img.Pix[i] = uint8(i / 4 % w * 255 / w)
		}
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return png.Encode(f, img)
	}
}

// writeScreenshot draws flat UI-like blocks, which compress far better than photo().
func writeScreenshot(w, h int) func(string) error {
	return func(path string) error {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				c := color.RGBA{245, 245, 245, 255}
				if (y/40)%3 == 0 && x%7 < 4 && x > w/10 {
					c = color.RGBA{30, 30, 30, 255}
				}
				if x < w/8 {
					c = color.RGBA{40, 90, 160, 255}
				}
				img.SetRGBA(x, y, c)
			}
		}
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return png.Encode(f, img)
	}
}

func writeGIF(w, h, frames int) func(string) error {
	return func(path string) error {
		anim := &gif.GIF{}
		for i := 0; i < frames; i++ {
			img := image.NewPaletted(image.Rect(0, 0, w, h), palette.Plan9)
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					img.Set(x, y, color.RGBA{uint8(x + i*8), uint8(y), uint8(i * 20), 255})
				}
			}
			anim.Image = append(anim.Image, img)
			anim.Delay = append(anim.Delay, 10)
		}
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return gif.EncodeAll(f, anim)
	}
}

func viaFFmpeg(t testing.TB, args ...string) func(string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found")
	}
	return func(path string) error {
		args := append([]string{"-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=size=300x200:rate=10:duration=1"}, args...)
		out, err := exec.Command("ffmpeg", append(args, path)...).CombinedOutput()
		if err != nil {
			return &exec.ExitError{Stderr: out}
		}
		return nil
	}
}

type webpInfo struct {
	width, height, frames int
	alpha                 bool
}

func readWebP(t testing.TB, path string) webpInfo {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		t.Fatalf("%s is not webp", path)
	}
	info := webpInfo{frames: 1}
	anmf := 0
	u24 := func(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 }
	for p := 12; p+8 <= len(data); {
		id := string(data[p : p+4])
		size := int(binary.LittleEndian.Uint32(data[p+4:]))
		body := data[p+8 : min(p+8+size, len(data))]
		switch id {
		case "VP8X":
			info.alpha = body[0]&0x10 != 0
			info.width = u24(body[4:]) + 1
			info.height = u24(body[7:]) + 1
		case "VP8 ":
			if info.width == 0 {
				info.width = int(binary.LittleEndian.Uint16(body[6:]) & 0x3fff)
				info.height = int(binary.LittleEndian.Uint16(body[8:]) & 0x3fff)
			}
		case "VP8L":
			if info.width == 0 {
				bits := binary.LittleEndian.Uint32(body[1:])
				info.width = int(bits&0x3fff) + 1
				info.height = int(bits>>14&0x3fff) + 1
				info.alpha = bits>>28&1 != 0
			}
		case "ANMF":
			anmf++
		}
		p += 8 + size + size%2
	}
	if anmf > 0 {
		info.frames = anmf
	}
	return info
}

func near(a, b int) bool { return a-b <= 1 && b-a <= 1 }

func TestResize(t *testing.T) {
	cases := []struct {
		name         string
		input        func(t testing.TB) string
		apng         bool
		w, h         int
		wantW, wantH int
		wantFrames   int
		wantAlpha    bool
	}{
		{name: "jpeg box", input: func(t testing.TB) string { return fixture(t, "photo.jpg", writeJPEG(4000, 3000, 0)) }, w: 256, h: 256, wantW: 256, wantH: 192, wantFrames: 1},
		{name: "jpeg width only", input: func(t testing.TB) string { return fixture(t, "photo.jpg", writeJPEG(4000, 3000, 0)) }, w: 256, wantW: 256, wantH: 192, wantFrames: 1},
		{name: "jpeg height only", input: func(t testing.TB) string { return fixture(t, "photo.jpg", writeJPEG(4000, 3000, 0)) }, h: 300, wantW: 400, wantH: 300, wantFrames: 1},
		{name: "jpeg no upscale", input: func(t testing.TB) string { return fixture(t, "small.jpg", writeJPEG(400, 200, 0)) }, w: 2000, h: 2000, wantW: 400, wantH: 200, wantFrames: 1},
		{name: "jpeg exif rotate", input: func(t testing.TB) string { return fixture(t, "rotated.jpg", writeJPEG(400, 200, 6)) }, w: 1000, h: 1000, wantW: 200, wantH: 400, wantFrames: 1},
		{name: "png alpha", input: func(t testing.TB) string { return fixture(t, "alpha.png", writePNG(800, 400)) }, w: 200, h: 200, wantW: 200, wantH: 100, wantFrames: 1, wantAlpha: true},
		{name: "gif animated", input: func(t testing.TB) string { return fixture(t, "anim.gif", writeGIF(300, 300, 10)) }, w: 100, h: 100, wantW: 100, wantH: 100, wantFrames: 10},
		{name: "webp animated", input: func(t testing.TB) string {
			return fixture(t, "anim.webp", viaFFmpeg(t, "-c:v", "libwebp", "-loop", "0"))
		}, w: 150, h: 150, wantW: 150, wantH: 100, wantFrames: 10},
		{name: "apng", input: func(t testing.TB) string {
			return fixture(t, "anim.apng", viaFFmpeg(t, "-f", "apng", "-plays", "0"))
		}, apng: true, w: 150, h: 150, wantW: 150, wantH: 100, wantFrames: 10},
		{name: "svg", input: func(t testing.TB) string {
			return fixture(t, "vector.svg", func(path string) error {
				return os.WriteFile(path, []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="400" height="200"><rect width="400" height="200" fill="#2a5"/></svg>`), 0644)
			})
		}, w: 200, h: 200, wantW: 200, wantH: 100, wantFrames: 1},
		{name: "plain png named apng", input: func(t testing.TB) string { return fixture(t, "alpha.png", writePNG(800, 400)) }, apng: true, w: 200, h: 200, wantW: 200, wantH: 100, wantFrames: 1, wantAlpha: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.input(t)
			out := filepath.Join(t.TempDir(), "out")
			if code := resizeImage(in, out, tc.w, tc.h, tc.apng); code != 0 {
				t.Fatalf("resize returned %d", code)
			}
			got := readWebP(t, out)
			if !near(got.width, tc.wantW) || !near(got.height, tc.wantH) {
				t.Errorf("size = %dx%d, want %dx%d", got.width, got.height, tc.wantW, tc.wantH)
			}
			if got.frames != tc.wantFrames {
				t.Errorf("frames = %d, want %d", got.frames, tc.wantFrames)
			}
			if tc.wantAlpha && !got.alpha {
				t.Errorf("alpha lost")
			}
		})
	}
}

func TestResizeBroken(t *testing.T) {
	in := filepath.Join(t.TempDir(), "broken")
	os.WriteFile(in, []byte("this is not an image"), 0644)
	if code := resizeImage(in, filepath.Join(t.TempDir(), "out"), 100, 100, false); code == 0 {
		t.Fatal("expected failure for broken input")
	}
}

// resetPeakRSS drops fixture generation from the process peak so peakRSS-MB reflects resizing only.
func resetPeakRSS(b *testing.B) {
	debug.FreeOSMemory()
	os.WriteFile("/proc/self/clear_refs", []byte("5"), 0)
	b.ResetTimer()
}

func reportPeakRSS(b *testing.B) {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(status), "\n") {
		if kb, ok := strings.CutPrefix(line, "VmHWM:"); ok {
			v, _ := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(kb, "kB")))
			b.ReportMetric(float64(v)/1024, "peakRSS-MB")
		}
	}
}

func benchResize(b *testing.B, in string, w, h int, apng bool) {
	out := filepath.Join(b.TempDir(), "out")
	resetPeakRSS(b)
	for i := 0; i < b.N; i++ {
		if code := resizeImage(in, out, w, h, apng); code != 0 {
			b.Fatalf("resize returned %d", code)
		}
	}
	reportPeakRSS(b)
}

func BenchmarkJPEG4000x3000(b *testing.B) {
	benchResize(b, fixture(b, "photo.jpg", writeJPEG(4000, 3000, 0)), 600, 0, false)
}

func BenchmarkPNG2000x2000(b *testing.B) {
	benchResize(b, fixture(b, "big.png", writePNG(2000, 2000)), 600, 0, false)
}

func BenchmarkPNGScreenshot2000x2000(b *testing.B) {
	benchResize(b, fixture(b, "shot.png", writeScreenshot(2000, 2000)), 600, 0, false)
}

func BenchmarkGIF500x500x30(b *testing.B) {
	benchResize(b, fixture(b, "big.gif", writeGIF(500, 500, 30)), 200, 200, false)
}

func BenchmarkAPNG(b *testing.B) {
	benchResize(b, fixture(b, "anim.apng", viaFFmpeg(b, "-f", "apng", "-plays", "0")), 150, 150, true)
}

func BenchmarkJPEGParallel(b *testing.B) {
	in := fixture(b, "photo.jpg", writeJPEG(4000, 3000, 0))
	dir := b.TempDir()
	resetPeakRSS(b)
	b.RunParallel(func(pb *testing.PB) {
		out, _ := os.CreateTemp(dir, "out")
		out.Close()
		for pb.Next() {
			if code := resizeImage(in, out.Name(), 600, 0, false); code != 0 {
				b.Errorf("resize returned %d", code)
			}
		}
	})
	reportPeakRSS(b)
}
