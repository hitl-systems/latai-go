// SPDX-License-Identifier: Apache-2.0 OR MIT

package latai

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const testTarget = "x86_64-unknown-linux-gnu"

type entry struct {
	name string
	body []byte
	typ  byte
}

func makeArchive(t *testing.T, entries []entry) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: e.typ}
		if e.typ == tar.TypeSymlink {
			h.Linkname, h.Size = "/etc/passwd", 0
		}
		if e.typ == tar.TypeDir {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			tw.Write(e.body)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// goodCore is a small archive laid out as `cargo xtask core-archive` lays one out.
func goodCore(t *testing.T, version, top string) []byte {
	lib := []byte("not really a library")
	manifest, _ := json.Marshal(map[string]any{
		"name": "latai-core", "version": version, "target": testTarget,
		"files": []map[string]any{{"path": "liblatai.so", "bytes": len(lib), "sha256": hexSHA(lib)}},
	})
	return makeArchive(t, []entry{
		{top + "/", nil, tar.TypeDir},
		{top + "/core.json", manifest, tar.TypeReg},
		{top + "/liblatai.so", lib, tar.TypeReg},
	})
}

func pinsFor(file string, archive []byte) *corePins {
	return &corePins{Version: "9.9.9", Targets: map[string]corePin{
		testTarget: {File: file, Bytes: int64(len(archive)), SHA256: hexSHA(archive)},
	}}
}

func serve(t *testing.T, files map[string][]byte) (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, ok := files[strings.TrimPrefix(r.URL.Path, "/v9.9.9/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestEnsureCoreDownloadsChecksUnpacksAndThenUsesTheCache(t *testing.T) {
	file := "latai-core-9.9.9-" + testTarget + ".tar.gz"
	top := strings.TrimSuffix(file, ".tar.gz")
	archive := goodCore(t, "9.9.9", top)
	srv, hits := serve(t, map[string][]byte{file: archive})
	cache := t.TempDir()
	dir, err := ensureCore(context.Background(), pinsFor(file, archive), testTarget, cache, srv.URL+"/v{version}/")
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(cache, "9.9.9", top) || !exists(filepath.Join(dir, "liblatai.so")) {
		t.Fatalf("%s", dir)
	}
	if _, err := ensureCore(context.Background(), pinsFor(file, archive), testTarget, cache, srv.URL+"/v{version}/"); err != nil || hits.Load() != 1 {
		t.Fatalf("%v, %d requests", err, hits.Load())
	}
	// A file changed in the cache is noticed and the core fetched again.
	must(os.WriteFile(filepath.Join(dir, "liblatai.so"), []byte("tampered"), 0o644))
	if _, err := ensureCore(context.Background(), pinsFor(file, archive), testTarget, cache, srv.URL+"/v{version}/"); err != nil || hits.Load() != 2 {
		t.Fatalf("%v, %d requests", err, hits.Load())
	}
	// No temporary folder is left.
	left, _ := filepath.Glob(filepath.Join(cache, "9.9.9", ".unpack-*"))
	if len(left) != 0 {
		t.Fatal(left)
	}
}

func TestEveryWrongArchiveIsRefusedAndNothingIsKept(t *testing.T) {
	file := "latai-core-9.9.9-" + testTarget + ".tar.gz"
	top := strings.TrimSuffix(file, ".tar.gz")
	good := goodCore(t, "9.9.9", top)
	lib := []byte("x")
	badManifest, _ := json.Marshal(map[string]any{"version": "9.9.9", "target": testTarget,
		"files": []map[string]any{{"path": "liblatai.so", "bytes": 1, "sha256": strings.Repeat("0", 64)}}})
	cases := map[string]struct {
		served []byte
		pins   *corePins
	}{
		"another sha256":   {good, &corePins{Version: "9.9.9", Targets: map[string]corePin{testTarget: {file, int64(len(good)), strings.Repeat("0", 64)}}}},
		"another size":     {append(append([]byte{}, good...), 0), pinsFor(file, good)},
		"a path outside":   {nil, nil},
		"a symlink":        {nil, nil},
		"a lying manifest": {makeArchive(t, []entry{{top + "/core.json", badManifest, tar.TypeReg}, {top + "/liblatai.so", lib, tar.TypeReg}}), nil},
		"not gzip":         {[]byte("plain text"), nil},
	}
	for name, c := range cases {
		served := c.served
		switch name {
		case "a path outside":
			served = makeArchive(t, []entry{{top + "/../escape", []byte("x"), tar.TypeReg}})
		case "a symlink":
			served = makeArchive(t, []entry{{top + "/link", nil, tar.TypeSymlink}})
		}
		pins := c.pins
		if pins == nil {
			pins = pinsFor(file, served)
		}
		srv, _ := serve(t, map[string][]byte{file: served})
		cache := t.TempDir()
		if _, err := ensureCore(context.Background(), pins, testTarget, cache, srv.URL+"/v{version}/"); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if exists(filepath.Join(cache, "9.9.9", top)) || exists(filepath.Join(cache, "9.9.9", "escape")) {
			t.Errorf("%s: something was kept", name)
		}
	}
	// A platform with no pinned core is an error, not a download.
	if _, err := ensureCore(context.Background(), pinsFor(file, good), "riscv64-unknown-linux-gnu", t.TempDir(), "http://127.0.0.1:1/"); err == nil {
		t.Fatal("no core for this target, and no error")
	}
}

func TestTheHostTargetOfEachBuiltPlatform(t *testing.T) {
	for goosarch, want := range map[string]string{
		"linux/amd64": "x86_64-unknown-linux-gnu", "darwin/arm64": "aarch64-apple-darwin",
		"windows/amd64": "x86_64-pc-windows-msvc", "plan9/386": "",
	} {
		p := strings.Split(goosarch, "/")
		if got := target(p[0], p[1]); got != want {
			t.Errorf("%s: %q", goosarch, got)
		}
	}
}

func TestTheEmbeddedPinsParse(t *testing.T) {
	p, err := loadPins()
	if err != nil || p.Version == "" {
		t.Fatalf("%v %+v", err, p)
	}
}
