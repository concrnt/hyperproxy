package main

/*
#include <stdlib.h>
#include "resize.h"
*/
import "C"
import "unsafe"

func init_resize(memoryLimit int) {
	cint := C.init_resize(C.int(memoryLimit))
	if cint != 0 {
		panic("Failed to initialize resize")
	}
}

var sem = make(chan struct{}, 1)

func resize(input, output string, width, height int) int {

	// limit to one resize operation at a time
	sem <- struct{}{}
	defer func() {
		<-sem
	}()

	cInput := C.CString(input)
	defer C.free(unsafe.Pointer(cInput))

	cOutput := C.CString(output)
	defer C.free(unsafe.Pointer(cOutput))

	cWidth := C.int(width)
	cHeight := C.int(height)

	cint := C.resize(cInput, cOutput, cWidth, cHeight)
	return int(cint)
}
