// SPDX-License-Identifier: Apache-2.0 OR MIT

package latai

/*
#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"runtime/cgo"
	"sync"
	"unsafe"
)

// Listener is a stream session on a speech pack (API version 2): push audio as it arrives, read
// `partial`, `committed`, `utterance` and `then` events, and one terminal `final` or `error`.
// Events reach the callback on the goroutine that calls Push or Close, before it returns.
type Listener struct {
	mu     sync.Mutex
	s      *C.LataiStream
	cb     *callback
	handle cgo.Handle
	rt     *Runtime
	ended  bool
}

// Listen opens a stream session. fn gets each event and returns false to end the stream (it
// still ends with one `final`, `finish: cancelled`). A stream that cannot open gives its one
// `error` event to fn and returns the error.
func (r *Runtime) Listen(open *StreamOpen, fn func(StreamEvent) bool) (*Listener, error) {
	body, err := json.Marshal(open)
	if err != nil {
		return nil, err
	}
	if v := C.lg_version(); v < 2 {
		return nil, fmt.Errorf("latai: liblatai serves API version %d; Listen needs 2", v)
	}
	l := &Listener{rt: r}
	l.cb, l.handle = newCallback(func(data []byte) bool {
		var ev StreamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return false
		}
		if ev.Final != nil || ev.Error != nil {
			l.ended = true
		}
		return fn(ev)
	})
	err = r.use(func(h *C.LataiRuntime) error {
		p, n := bytesArg(body)
		defer C.free(unsafe.Pointer(p))
		var e C.lg_str
		if rc := C.lg_stream_open(h, p, n, C.uintptr_t(l.handle), &l.s, &e); rc != 0 {
			return wireError(int32(rc), takeBytes(e))
		}
		return nil
	})
	if l.cb.panicked != nil {
		l.handle.Delete()
		panic(l.cb.panicked)
	}
	if err != nil {
		l.handle.Delete()
		return nil, err
	}
	return l, nil
}

func (l *Listener) check() {
	if p := l.cb.panicked; p != nil {
		l.cb.panicked = nil
		panic(p)
	}
}

// Push gives the stream the next bytes of audio (raw 16-bit little-endian mono samples, or the
// bytes of a WAV file for `format: wav`), at most 1 MiB a call.
func (l *Listener) Push(audio []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.s == nil {
		return fmt.Errorf("latai: the stream is closed")
	}
	var e C.lg_str
	var p *C.uint8_t
	if len(audio) > 0 {
		p = (*C.uint8_t)(C.CBytes(audio))
		defer C.free(unsafe.Pointer(p))
	}
	rc := C.lg_stream_push(l.s, p, C.size_t(len(audio)), &e)
	l.check()
	if rc != 0 {
		return wireError(int32(rc), takeBytes(e))
	}
	return nil
}

// Cancel stops the stream from any goroutine; its `final` (cancelled) comes with the next Push
// or Close.
func (l *Listener) Cancel() error {
	l.mu.Lock()
	s := l.s
	l.mu.Unlock()
	if s == nil {
		return nil
	}
	var e C.lg_str
	if rc := C.lg_stream_cancel(s, &e); rc != 0 {
		return wireError(int32(rc), takeBytes(e))
	}
	return nil
}

// Close ends the stream: with flush the audio is over and the last utterance is decoded
// (`finish: stop`); without it the stream is abandoned (`cancelled`). Closing twice does nothing.
func (l *Listener) Close(flush bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.s == nil {
		return nil
	}
	f := C.int32_t(0)
	if flush {
		f = 1
	}
	var e C.lg_str
	rc := C.lg_stream_close(l.s, f, &e)
	l.s = nil
	defer l.handle.Delete()
	l.check()
	if rc != 0 {
		return wireError(int32(rc), takeBytes(e))
	}
	return nil
}
