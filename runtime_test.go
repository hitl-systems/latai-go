// SPDX-License-Identifier: Apache-2.0 OR MIT

package latai

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The integration tests run on the fixture packs of latai (`cargo xtask fetch`) with a liblatai
// (LATAI_LIB, or a checkout's target/debug). Without them they skip, unless
// LATAI_REQUIRE_FIXTURES=1. One runtime per process: TestMain opens it.
var (
	rt       *Runtime
	skipWhy  string
	fixtures string
)

const (
	textPack   = "x4r-l8"
	speechPack = "speech-qwen2a"
)

func fixturesDir() string {
	if d := os.Getenv("LATAI_FIXTURES_DIR"); d != "" {
		return d
	}
	return filepath.Join("..", "..", "tests", "fixtures", "cache")
}

func testKey() string {
	if k := os.Getenv("LATAI_TEST_PUB"); k != "" {
		return k
	}
	return filepath.Join("..", "..", "tests", "fixtures", "keys", "test.pub")
}

func TestMain(m *testing.M) {
	fixtures = fixturesDir()
	code := func() int {
		pack := filepath.Join(fixtures, "packs", textPack+".gguf")
		key, err := os.ReadFile(testKey())
		if _, perr := os.Stat(pack); perr != nil || err != nil {
			skipWhy = "no fixture pack or test key (cargo xtask fetch)"
			return m.Run()
		}
		data, err := os.MkdirTemp("", "latai-go-test-")
		if err != nil {
			panic(err)
		}
		defer os.RemoveAll(data)
		must(os.MkdirAll(filepath.Join(data, "keys", "packs"), 0o755))
		must(os.WriteFile(filepath.Join(data, "keys", "packs", "test.pub"), key, 0o644))
		must(os.WriteFile(filepath.Join(data, "latai.toml"), []byte("[runtime]\nchannel = \"dev\"\n"), 0o644))
		rt, err = Open(&Options{DataDir: data})
		if err != nil {
			skipWhy = "cannot open the runtime: " + err.Error()
			return m.Run()
		}
		defer rt.Close()
		if _, err := rt.Install(pack, true); err != nil {
			panic(err)
		}
		if sp := filepath.Join(fixtures, "speech-packs", speechPack+".gguf"); exists(sp) {
			if _, err := rt.Install(sp, true); err != nil {
				panic(err)
			}
		}
		return m.Run()
	}()
	os.Exit(code)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func need(t *testing.T) {
	t.Helper()
	if rt == nil {
		if os.Getenv("LATAI_REQUIRE_FIXTURES") == "1" {
			t.Fatal(skipWhy)
		}
		t.Skip(skipWhy)
	}
}

func ptr[T any](v T) *T { return &v }

func TestInfoNamesTheLibraryAndItsVersion(t *testing.T) {
	need(t)
	info, err := rt.Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Version == "" || LibraryPath() == "" {
		t.Fatalf("%+v %q", info, LibraryPath())
	}
	t.Logf("liblatai %s from %s", info.Version, LibraryPath())
}

func TestARunAnswersText(t *testing.T) {
	need(t)
	req := TextRequest(textPack, "hello")
	req.Output = &OutputOptions{MaxTokens: ptr(uint32(4))}
	resp, err := rt.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Output.Text == nil || resp.Finish != FinishLength || resp.Latai.Timing.GenTokens != 4 {
		t.Fatalf("%+v", resp)
	}
}

func TestAStreamGivesDeltasThenOneFinalWithTheSameText(t *testing.T) {
	need(t)
	req := TextRequest(textPack, "hello")
	req.Output = &OutputOptions{MaxTokens: ptr(uint32(6))}
	var text strings.Builder
	deltas, finals := 0, 0
	resp, err := rt.RunStream(context.Background(), req, func(ev Event) bool {
		switch {
		case ev.Delta != nil:
			deltas++
			text.WriteString(ev.Delta.Text)
		case ev.Final != nil:
			finals++
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if deltas == 0 || finals != 1 || resp.Text() != text.String() {
		t.Fatalf("%d deltas, %d finals, %q vs %q", deltas, finals, resp.Text(), text.String())
	}
	// The blocking run says the same.
	blocking, err := rt.Run(context.Background(), req)
	if err != nil || blocking.Text() != resp.Text() {
		t.Fatalf("%v %q %q", err, blocking.Text(), resp.Text())
	}
}

func TestBreakingOutOfEventsCancelsTheRun(t *testing.T) {
	need(t)
	req := TextRequest(textPack, "hello")
	req.Output = &OutputOptions{MaxTokens: ptr(uint32(200))}
	n := 0
	for ev, err := range rt.Events(context.Background(), req) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Delta != nil {
			n++
			if n == 2 {
				break
			}
		}
	}
	if n != 2 {
		t.Fatalf("%d deltas", n)
	}
	// The runtime still answers.
	if _, err := rt.Run(context.Background(), TextRequest(textPack, "again")); err != nil {
		t.Fatal(err)
	}
}

func TestACancelledContextEndsTheRunCancelled(t *testing.T) {
	need(t)
	ctx, cancel := context.WithCancel(context.Background())
	req := TextRequest(textPack, "hello")
	req.Output = &OutputOptions{MaxTokens: ptr(uint32(500))}
	started := make(chan struct{})
	var once bool
	done := make(chan *Response, 1)
	go func() {
		resp, err := rt.RunStream(ctx, req, func(ev Event) bool {
			if ev.Delta != nil && !once {
				once = true
				close(started)
			}
			return true
		})
		if err != nil {
			t.Error(err)
		}
		done <- resp
	}()
	select {
	case <-started:
	case <-time.After(60 * time.Second):
		t.Fatal("no delta")
	}
	cancel()
	resp := <-done
	if resp == nil || resp.Finish != FinishCancelled {
		t.Fatalf("%+v", resp)
	}
}

func TestErrorsAreTyped(t *testing.T) {
	need(t)
	resp, err := rt.Run(context.Background(), TextRequest("no-such-pack", "x"))
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeUnknownPack || e.Hint == "" || e.Response == nil || resp == nil {
		t.Fatalf("%v %+v", err, resp)
	}
	if resp.Finish != FinishError || resp.Latai.Started == nil || *resp.Latai.Started {
		t.Fatalf("%+v", resp)
	}
	// A request that does not parse is E0301 with no response.
	raw, err := rt.RunJSON(context.Background(), []byte(`{"pack":`))
	if !HasCode(err, CodeBadRequest) || raw != nil {
		t.Fatalf("%v %s", err, raw)
	}
	// A pack call fails with its code.
	if _, err := rt.Inspect("/no/such/file.gguf"); err == nil {
		t.Fatal("inspect of a missing file worked")
	}
	// Events yields an error that kept a stream from starting.
	for ev, err := range rt.Events(context.Background(), TextRequest("no-such-pack", "x")) {
		if ev.Error == nil && err == nil {
			t.Fatalf("%+v", ev)
		}
	}
}

func TestPackManagement(t *testing.T) {
	need(t)
	versions, err := rt.List()
	if err != nil || len(versions) == 0 {
		t.Fatalf("%v %v", err, versions)
	}
	st, err := rt.Status(textPack)
	if err != nil || st.State != StatusStateAvailable {
		t.Fatalf("%v %+v", err, st)
	}
	info, err := rt.Inspect(textPack)
	if err != nil || info.Name != textPack {
		t.Fatalf("%v %+v", err, info)
	}
	pin, err := rt.Pin(textPack, "channel:dev")
	if err != nil || pin != "channel:dev" {
		t.Fatalf("%v %q", err, pin)
	}
	if pin, err := rt.Pin(textPack, ""); err != nil || pin != "" {
		t.Fatalf("%v %q", err, pin)
	}
}

// pcm16 reads a fixture clip (float32 little-endian) as 16-bit samples.
func pcm16(t *testing.T, name string) []byte {
	raw, err := os.ReadFile(filepath.Join(fixtures, "speech", "clip-"+name+".f32le"))
	if err != nil {
		t.Skip("no speech clip fixture")
	}
	out := make([]byte, 0, len(raw)/2)
	for i := 0; i+4 <= len(raw); i += 4 {
		f := math.Float32frombits(binary.LittleEndian.Uint32(raw[i:]))
		s := int16(max(-32768, min(32767, math.Round(float64(f)*32767))))
		out = binary.LittleEndian.AppendUint16(out, uint16(s))
	}
	return out
}

func TestListenTranscribesPushedAudio(t *testing.T) {
	need(t)
	if _, err := rt.Status(speechPack); err != nil {
		t.Skip("no speech pack")
	}
	clip := pcm16(t, "sine")
	var finals int
	var final *StreamEventFinal
	l, err := rt.Listen(&StreamOpen{Pack: speechPack}, func(ev StreamEvent) bool {
		if ev.Final != nil {
			finals++
			final = ev.Final
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	for len(clip) > 0 {
		n := min(len(clip), 16000)
		if err := l.Push(clip[:n]); err != nil {
			t.Fatal(err)
		}
		clip = clip[n:]
	}
	if err := l.Close(true); err != nil {
		t.Fatal(err)
	}
	if finals != 1 || final.Response.Finish != FinishStop {
		t.Fatalf("%d finals %+v", finals, final)
	}
	if err := l.Close(true); err != nil {
		t.Fatal("a second close must do nothing")
	}
}

func TestAPanicInTheCallbackComesBackAfterTheCall(t *testing.T) {
	need(t)
	req := TextRequest(textPack, "hello")
	req.Output = &OutputOptions{MaxTokens: ptr(uint32(20))}
	defer func() {
		if p := recover(); p != "boom" {
			t.Fatalf("recovered %v", p)
		}
		// The runtime survived.
		if _, err := rt.Run(context.Background(), TextRequest(textPack, "x")); err != nil {
			t.Fatal(err)
		}
	}()
	_, _ = rt.RunStream(context.Background(), req, func(ev Event) bool { panic("boom") })
}
