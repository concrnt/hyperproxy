package main

/*
#cgo pkg-config: vips
#include <stdlib.h>
#include <vips/vips.h>
#include "resize.h"
*/
import "C"

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"time"
	"unsafe"
)

var sem chan struct{}

func init_resize() {
	if C.init_resize() != 0 {
		panic("Failed to initialize resize")
	}

	n := runtime.GOMAXPROCS(0)
	if v, err := strconv.Atoi(os.Getenv("HYPERPROXY_RESIZE_CONCURRENCY")); err == nil && v > 0 {
		n = v
	}
	sem = make(chan struct{}, n)
}

// resizeImage converts input to WebP fitting within width x height (0 = unbounded), never upscaling.
func resizeImage(input, output string, width, height int, apng bool) int {
	sem <- struct{}{}
	defer func() {
		<-sem
	}()

	// libvips reads only the first frame of APNG, so keep the animation via ffmpeg.
	if apng {
		err := resizeAPNG(input, output, width, height)
		if err == nil {
			return 0
		}
		fmt.Println("    apng conversion failed, falling back to still image:", err)
	}

	cInput := C.CString(input)
	defer C.free(unsafe.Pointer(cInput))

	cOutput := C.CString(output)
	defer C.free(unsafe.Pointer(cOutput))

	return int(C.resize(cInput, cOutput, C.int(width), C.int(height)))
}

func resizeAPNG(input, output string, width, height int) error {
	bound := func(v int) string {
		if v <= 0 {
			return strconv.Itoa(int(C.VIPS_MAX_COORD))
		}
		return strconv.Itoa(v)
	}
	scale := fmt.Sprintf("scale=w='min(iw,%s)':h='min(ih,%s)':force_original_aspect_ratio=decrease", bound(width), bound(height))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-nostdin", "-y",
		"-f", "apng", "-i", input,
		"-vf", scale,
		"-c:v", "libwebp", "-quality", "70", "-loop", "0",
		"-f", "webp", output,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}
