// SPDX-License-Identifier: Apache-2.0 OR MIT

package latai

/*
#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sync"
	"sync/atomic"
	"unsafe"
)

// Options of Open. Every field may be empty.
type Options struct {
	// The data directory (packs, keys, latai.toml). Default: $LATAI_DATA_DIR, then the OS one.
	DataDir string `json:"data_dir,omitempty"`
	// The folder of the engine's libraries (lib/latai). Default: the one beside liblatai.
	BundleDir string `json:"bundle_dir,omitempty"`
	// liblatai itself: a file or a folder (LibraryCandidates). Not sent to the core.
	Library string `json:"-"`
}

// Runtime is the one latai runtime of this process. Its methods are safe for concurrent use.
type Runtime struct {
	mu     sync.RWMutex
	h      *C.LataiRuntime
	closed bool
}

var (
	openMu  sync.Mutex
	nextID  atomic.Uint64
	errDone = errors.New("latai: the runtime is closed")
)

// Open loads liblatai (once per process) and opens the runtime. One runtime per process: a
// second Open before Close is E0901.
func Open(opts *Options) (*Runtime, error) {
	if opts == nil {
		opts = &Options{}
	}
	if err := load(opts.Library); err != nil {
		return nil, err
	}
	cfg, err := json.Marshal(opts)
	if err != nil {
		return nil, err
	}
	if string(cfg) == "{}" {
		cfg = nil
	}
	openMu.Lock()
	defer openMu.Unlock()
	p, n := bytesArg(cfg)
	defer C.free(unsafe.Pointer(p))
	var h *C.LataiRuntime
	var e C.lg_str
	if rc := C.lg_open(p, n, &h, &e); rc != 0 {
		return nil, wireError(int32(rc), takeBytes(e))
	}
	return &Runtime{h: h}, nil
}

// Close stops the requests in flight, answers queued ones `cancelled` and releases the runtime.
// Closing twice returns nil.
func (r *Runtime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	var e C.lg_str
	if rc := C.lg_close(r.h, &e); rc != 0 {
		return wireError(int32(rc), takeBytes(e))
	}
	return nil
}

// use runs f with the handle while the runtime is open.
func (r *Runtime) use(f func(h *C.LataiRuntime) error) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return errDone
	}
	return f(r.h)
}

// Info is the runtime's info (version, thread budget, engine variant, loaded packs, plug-ins).
func (r *Runtime) Info() (*RuntimeInfo, error) {
	var out RuntimeInfo
	err := r.use(func(h *C.LataiRuntime) error {
		var o, e C.lg_str
		if rc := C.lg_info(h, &o, &e); rc != 0 {
			return wireError(int32(rc), takeBytes(e))
		}
		return json.Unmarshal(takeBytes(o), &out)
	})
	return &out, err
}

func newRequestID() string { return fmt.Sprintf("go-%d", nextID.Add(1)) }

// watch cancels request id when ctx ends; stop it once the call returned.
func (r *Runtime) watch(ctx context.Context, id string) (stop func()) {
	if ctx.Done() == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = r.Cancel(id)
		case <-done:
		}
	}()
	return func() { close(done) }
}

// RunJSON runs a wire request given as JSON and returns the response JSON. When the request
// reached the core the response is returned even if the run failed; err is then the response's
// error.
func (r *Runtime) RunJSON(ctx context.Context, request []byte) ([]byte, error) {
	id := newRequestID()
	stop := r.watch(ctx, id)
	defer stop()
	var resp []byte
	err := r.use(func(h *C.LataiRuntime) error {
		p, n := bytesArg(request)
		defer C.free(unsafe.Pointer(p))
		cid := C.CString(id)
		defer C.free(unsafe.Pointer(cid))
		var o, e C.lg_str
		rc := C.lg_run(h, p, n, cid, &o, &e)
		resp = takeBytes(o)
		errJSON := takeBytes(e)
		if rc != 0 {
			return wireError(int32(rc), errJSON)
		}
		return nil
	})
	return resp, err
}

// Run runs a request and waits. A run that ends `finish: error` returns its response and an
// *Error (with Response set); a cancelled run (ctx or Cancel) returns `finish: cancelled` and no
// error.
func (r *Runtime) Run(ctx context.Context, req *Request) (*Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	raw, runErr := r.RunJSON(ctx, body)
	if raw == nil {
		return nil, runErr
	}
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("latai: the response is not wire JSON: %w", err)
	}
	if resp.Error != nil {
		return &resp, &Error{Code: resp.Error.Code, Message: resp.Error.Message, Hint: resp.Error.Hint, Details: detailsJSON(resp.Error.Details), Response: &resp}
	}
	return &resp, nil
}

func detailsJSON(d map[string]json.RawMessage) json.RawMessage {
	if len(d) == 0 {
		return nil
	}
	b, _ := json.Marshal(d)
	return b
}

// RunStream runs a request with `stream: true` and gives every event to fn on this goroutine:
// deltas, then one `final` (or one `error`). fn returning false cancels the run, which still
// ends with its `final`; fn is not called after it returned false. The return is the response of
// the `final` event, or the error of the `error` event. fn must not call Run, RunStream, Close or
// a pack call (E0303); Cancel is fine.
func (r *Runtime) RunStream(ctx context.Context, req *Request, fn func(Event) bool) (*Response, error) {
	streamed := *req
	on := true
	streamed.Stream = &on
	body, err := json.Marshal(&streamed)
	if err != nil {
		return nil, err
	}
	id := newRequestID()
	stop := r.watch(ctx, id)
	defer stop()
	var final *Response
	var terminal error
	var decodeErr error
	stopped := false
	cb, handle := newCallback(func(data []byte) bool {
		var ev Event
		if err := json.Unmarshal(data, &ev); err != nil {
			decodeErr = err
			return false
		}
		switch {
		case ev.Final != nil:
			final = &ev.Final.Response
		case ev.Error != nil:
			terminal = &Error{Code: ev.Error.Error.Code, Message: ev.Error.Error.Message, Hint: ev.Error.Error.Hint, Details: detailsJSON(ev.Error.Error.Details)}
		}
		if stopped || fn == nil {
			return !stopped
		}
		if !fn(ev) {
			stopped = true
		}
		return !stopped
	})
	defer handle.Delete()
	err = r.use(func(h *C.LataiRuntime) error {
		p, n := bytesArg(body)
		defer C.free(unsafe.Pointer(p))
		cid := C.CString(id)
		defer C.free(unsafe.Pointer(cid))
		var e C.lg_str
		rc := C.lg_run_stream(h, p, n, cid, C.uintptr_t(handle), &e)
		errJSON := takeBytes(e)
		if rc != 0 && terminal == nil && final == nil {
			return wireError(int32(rc), errJSON)
		}
		return nil
	})
	if cb.panicked != nil {
		panic(cb.panicked)
	}
	switch {
	case err != nil:
		return nil, err
	case decodeErr != nil:
		return final, fmt.Errorf("latai: an event is not wire JSON: %w", decodeErr)
	case terminal != nil:
		return nil, terminal
	case final != nil && final.Error != nil:
		return final, &Error{Code: final.Error.Code, Message: final.Error.Message, Hint: final.Error.Hint, Details: detailsJSON(final.Error.Details), Response: final}
	}
	return final, nil
}

// Events runs a request as a stream and yields its events in order; breaking out of the loop
// cancels the run (it still ends in the core with its `final`). The last pair is the `final` or
// `error` event; when the run could not start at all (no event came), it is a zero Event and the
// error. The loop body runs inside the call into the core: do not call Run, RunStream, Close or a
// pack call from it.
func (r *Runtime) Events(ctx context.Context, req *Request) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		more, terminal := true, false
		_, err := r.RunStream(ctx, req, func(ev Event) bool {
			terminal = ev.Final != nil || ev.Error != nil
			more = yield(ev, nil)
			return more
		})
		if err != nil && more && !terminal {
			yield(Event{}, err)
		}
	}
}

// Cancel cancels a request by its id; an unknown or finished id does nothing.
func (r *Runtime) Cancel(id string) error {
	return r.use(func(h *C.LataiRuntime) error {
		cid := C.CString(id)
		defer C.free(unsafe.Pointer(cid))
		var e C.lg_str
		if rc := C.lg_cancel(h, cid, &e); rc != 0 {
			return wireError(int32(rc), takeBytes(e))
		}
		return nil
	})
}

// SessionClose frees a session's KV slot; its next request runs cold.
func (r *Runtime) SessionClose(session string) error {
	return r.use(func(h *C.LataiRuntime) error {
		cs := C.CString(session)
		defer C.free(unsafe.Pointer(cs))
		var e C.lg_str
		if rc := C.lg_session_close(h, cs, &e); rc != 0 {
			return wireError(int32(rc), takeBytes(e))
		}
		return nil
	})
}

// call1 runs a pack call that returns one JSON value into out.
func (r *Runtime) call1(out any, f func(h *C.LataiRuntime, o, e *C.lg_str) C.int32_t) error {
	return r.use(func(h *C.LataiRuntime) error {
		var o, e C.lg_str
		rc := f(h, &o, &e)
		body := takeBytes(o)
		errJSON := takeBytes(e)
		if rc != 0 {
			return wireError(int32(rc), errJSON)
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(body, out)
	})
}

func withC(s string, f func(*C.char) C.int32_t) C.int32_t {
	c := C.CString(s)
	defer C.free(unsafe.Pointer(c))
	return f(c)
}

// Inspect reads what a pack says about itself (a file, or the name of an installed pack).
func (r *Runtime) Inspect(target string) (*PackInfo, error) {
	var out PackInfo
	err := r.call1(&out, func(h *C.LataiRuntime, o, e *C.lg_str) C.int32_t {
		return withC(target, func(c *C.char) C.int32_t { return C.lg_call1(0, h, c, o, e) })
	})
	return &out, err
}

// Stage checks a pack file and its signature and stores it, not activated.
func (r *Runtime) Stage(path string) (*StageReceipt, error) {
	var out StageReceipt
	err := r.call1(&out, func(h *C.LataiRuntime, o, e *C.lg_str) C.int32_t {
		return withC(path, func(c *C.char) C.int32_t { return C.lg_call1(1, h, c, o, e) })
	})
	return &out, err
}

// Pull downloads and stages the pack at url; force ignores the `[pull]` download rules.
func (r *Runtime) Pull(url string, force bool) (*StageReceipt, error) {
	var out StageReceipt
	f := C.int32_t(0)
	if force {
		f = 1
	}
	err := r.call1(&out, func(h *C.LataiRuntime, o, e *C.lg_str) C.int32_t {
		return withC(url, func(c *C.char) C.int32_t { return C.lg_pull(h, c, f, o, e) })
	})
	return &out, err
}

// Install stages a pack file (or every pack of a folder) and activates it unless activate is false.
func (r *Runtime) Install(source string, activate bool) ([]StageReceipt, error) {
	var out []StageReceipt
	a := C.int32_t(0)
	if activate {
		a = 1
	}
	err := r.call1(&out, func(h *C.LataiRuntime, o, e *C.lg_str) C.int32_t {
		return withC(source, func(c *C.char) C.int32_t { return C.lg_install(h, c, a, o, e) })
	})
	return out, err
}

// Activate makes a version of name serve; version "" is the newest the pin allows.
func (r *Runtime) Activate(name, version string) (*PackVersion, error) {
	var out PackVersion
	var v *string
	if version != "" {
		v = &version
	}
	err := r.call1(&out, func(h *C.LataiRuntime, o, e *C.lg_str) C.int32_t {
		cn, cv := C.CString(name), cstr(v)
		defer freeC(cn)
		defer freeC(cv)
		return C.lg_activate(h, cn, cv, o, e)
	})
	return &out, err
}

// Rollback brings the retained version of name back.
func (r *Runtime) Rollback(name string) (*PackVersion, error) {
	var out PackVersion
	err := r.call1(&out, func(h *C.LataiRuntime, o, e *C.lg_str) C.int32_t {
		return withC(name, func(c *C.char) C.int32_t { return C.lg_call1(2, h, c, o, e) })
	})
	return &out, err
}

// List lists every installed version with its state and pin.
func (r *Runtime) List() ([]PackVersion, error) {
	var out []PackVersion
	err := r.call1(&out, func(h *C.LataiRuntime, o, e *C.lg_str) C.int32_t { return C.lg_list(h, o, e) })
	return out, err
}

// Pin pins name to a sha256 or `channel:NAME`; pin "" clears it. The return is the pin in force
// ("" after a clear).
func (r *Runtime) Pin(name, pin string) (string, error) {
	var out *string
	var p *string
	if pin != "" {
		p = &pin
	}
	err := r.call1(&out, func(h *C.LataiRuntime, o, e *C.lg_str) C.int32_t {
		cn, cp := C.CString(name), cstr(p)
		defer freeC(cn)
		defer freeC(cp)
		return C.lg_pin(h, cn, cp, o, e)
	})
	if out == nil {
		return "", err
	}
	return *out, err
}

// Status is the state of a pack from local state only.
func (r *Runtime) Status(name string) (*Status, error) {
	var out Status
	err := r.call1(&out, func(h *C.LataiRuntime, o, e *C.lg_str) C.int32_t {
		return withC(name, func(c *C.char) C.int32_t { return C.lg_call1(3, h, c, o, e) })
	})
	return &out, err
}

func encodeBase64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
