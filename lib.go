// SPDX-License-Identifier: Apache-2.0 OR MIT

package latai

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo linux LDFLAGS: -ldl
#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"unsafe"
)

// APIVersion is the version of the C table this SDK is written for (include/latai.h).
const APIVersion = 2

var (
	loadOnce sync.Once
	loadErr  error
	loadedAt string
)

// libraryNames are the file names liblatai has on this platform.
func libraryNames(goos string) []string {
	switch goos {
	case "windows":
		return []string{"latai.dll", "liblatai.dll"}
	case "darwin", "ios":
		return []string{"liblatai.dylib"}
	}
	return []string{"liblatai.so"}
}

// LibraryCandidates lists where liblatai is looked for, in order (nothing is read):
//
//  1. explicit (Options.Library: a file, or a folder holding the library);
//  2. $LATAI_LIB (a file or a folder), then $LATAI_LIB_DIR (a folder);
//  3. the core that `go run .../cmd/latai-core` put in the user cache for this SDK's version;
//  4. a source checkout of latai: <repo>/target/debug and <repo>/target/release;
//  5. the bare file names, which the dynamic loader resolves.
func LibraryCandidates(explicit string) []string {
	names := libraryNames(goruntime.GOOS)
	var out []string
	add := func(p string) {
		for _, have := range out {
			if have == p {
				return
			}
		}
		if p != "" {
			out = append(out, p)
		}
	}
	fileOrDir := func(p string) {
		if p == "" {
			return
		}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			for _, n := range names {
				add(filepath.Join(p, n))
			}
			return
		}
		add(p)
	}
	fileOrDir(explicit)
	fileOrDir(os.Getenv("LATAI_LIB"))
	if d := os.Getenv("LATAI_LIB_DIR"); d != "" {
		for _, n := range names {
			add(filepath.Join(d, n))
		}
	}
	if d, err := CoreDir(); err == nil {
		for _, n := range names {
			add(filepath.Join(d, n))
		}
	}
	if _, file, _, ok := goruntime.Caller(0); ok {
		repo := filepath.Join(filepath.Dir(file), "..", "..")
		if _, err := os.Stat(filepath.Join(repo, "include", "latai.h")); err == nil {
			for _, profile := range []string{"debug", "release"} {
				for _, n := range names {
					add(filepath.Join(repo, "target", profile, n))
				}
			}
		}
	}
	for _, n := range names {
		add(n)
	}
	return out
}

// load opens liblatai once per process: the first candidate that exists (a bare name is
// tried as it is).
func load(explicit string) error {
	loadOnce.Do(func() {
		tried := LibraryCandidates(explicit)
		var last string
		for _, p := range tried {
			if strings.ContainsRune(p, filepath.Separator) {
				if _, err := os.Stat(p); err != nil {
					continue
				}
			}
			cpath := C.CString(p)
			var e C.lg_str
			rc := C.lg_load(cpath, APIVersion, &e)
			C.free(unsafe.Pointer(cpath))
			if rc == 0 {
				loadedAt = p
				return
			}
			last = take(e)
		}
		loadErr = &LibraryError{Tried: tried, Cause: orDefault(last, "not found")}
	})
	return loadErr
}

// LibraryPath is the file liblatai was loaded from ("" before Open).
func LibraryPath() string { return loadedAt }

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// take copies a shim string into Go memory and frees it.
func takeBytes(s C.lg_str) []byte {
	if s.ptr == nil {
		return nil
	}
	b := C.GoBytes(unsafe.Pointer(s.ptr), C.int(s.len))
	C.lg_free(unsafe.Pointer(s.ptr))
	return b
}

func take(s C.lg_str) string { return string(takeBytes(s)) }

// cstr is a C copy of s, or NULL for nil; free with freeC.
func cstr(s *string) *C.char {
	if s == nil {
		return nil
	}
	return C.CString(*s)
}

func freeC(p *C.char) {
	if p != nil {
		C.free(unsafe.Pointer(p))
	}
}

// bytesArg is (pointer, length) of b for the C side; the pointer is NULL for an empty slice.
func bytesArg(b []byte) (*C.char, C.size_t) {
	if len(b) == 0 {
		return nil, 0
	}
	return (*C.char)(C.CBytes(b)), C.size_t(len(b))
}
