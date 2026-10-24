// SPDX-License-Identifier: Apache-2.0 OR MIT

package latai

/*
#include <stddef.h>
#include <stdint.h>
*/
import "C"

import (
	"runtime/cgo"
	"unsafe"
)

// eventFunc gets one event's JSON (a copy) and says whether to go on.
type eventFunc func(event []byte) bool

// goLataiEvent is the callback of run_stream and stream_open (shim.c's trampoline): the handle
// names an eventFunc. A panic in the callback must not unwind into C, so it is caught, kept, and
// stops the run; the caller re-panics once the C call has returned.
//
//export goLataiEvent
func goLataiEvent(handle C.uintptr_t, data *C.char, n C.size_t) C.int32_t {
	cb := cgo.Handle(handle).Value().(*callback)
	if cb.panicked != nil {
		return 1
	}
	event := C.GoBytes(unsafe.Pointer(data), C.int(n))
	keep := func() (keep bool) {
		defer func() {
			if p := recover(); p != nil {
				cb.panicked = p
				keep = false
			}
		}()
		return cb.fn(event)
	}()
	if keep {
		return 0
	}
	return 1
}

// callback holds an eventFunc for the time of one C call (or one stream session).
type callback struct {
	fn       eventFunc
	panicked any
}

func newCallback(fn eventFunc) (*callback, cgo.Handle) {
	cb := &callback{fn: fn}
	return cb, cgo.NewHandle(cb)
}
