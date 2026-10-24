// SPDX-License-Identifier: Apache-2.0 OR MIT

package latai

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

// The core this SDK version runs: one archive per target, by size and sha256 (`cargo xtask
// core-archive` writes the file; a release copies it here).
//
//go:embed core-pins.json
var pinsJSON []byte

// CoreBaseURL is where the archives of the pinned version are published; `{version}` is replaced.
// LATAI_CORE_URL overrides it (a mirror, or a folder served for tests).
var CoreBaseURL = "https://github.com/hitl-systems/latai-go/releases/download/v{version}/"

// The limits of an archive, far above a real one (about 10 MB packed, 25 MB unpacked).
const (
	maxArchiveBytes  = 256 << 20
	maxUnpackedBytes = 1 << 30
	maxEntries       = 4096
)

type corePins struct {
	Version string             `json:"version"`
	Targets map[string]corePin `json:"targets"`
}

type corePin struct {
	File   string `json:"file"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type coreManifest struct {
	Version string `json:"version"`
	Target  string `json:"target"`
	Files   []struct {
		Path   string `json:"path"`
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

// HostTarget is the core target of this platform (a Rust target triple), "" when none is built.
func HostTarget() string {
	return target(goruntime.GOOS, goruntime.GOARCH)
}

func target(goos, goarch string) string {
	switch goos + "/" + goarch {
	case "linux/amd64":
		return "x86_64-unknown-linux-gnu"
	case "linux/arm64":
		return "aarch64-unknown-linux-gnu"
	case "darwin/arm64":
		return "aarch64-apple-darwin"
	case "darwin/amd64":
		return "x86_64-apple-darwin"
	case "windows/amd64":
		return "x86_64-pc-windows-msvc"
	case "windows/arm64":
		return "aarch64-pc-windows-msvc"
	}
	return ""
}

func loadPins() (*corePins, error) {
	var p corePins
	if err := json.Unmarshal(pinsJSON, &p); err != nil {
		return nil, fmt.Errorf("latai: core-pins.json: %w", err)
	}
	return &p, nil
}

func cacheRoot() (string, error) {
	if d := os.Getenv("LATAI_CORE_CACHE"); d != "" {
		return d, nil
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "latai", "core"), nil
}

// CoreDir is the folder of this SDK's pinned core for this platform in the user cache (it may
// not exist yet: EnsureCore fills it).
func CoreDir() (string, error) {
	pins, err := loadPins()
	if err != nil {
		return "", err
	}
	root, err := cacheRoot()
	if err != nil {
		return "", err
	}
	return coreDir(pins, root, HostTarget())
}

func coreDir(pins *corePins, root, tgt string) (string, error) {
	pin, ok := pins.Targets[tgt]
	if !ok {
		return "", fmt.Errorf("latai: no core %s for %s", pins.Version, orDefault(tgt, goruntime.GOOS+"/"+goruntime.GOARCH))
	}
	return filepath.Join(root, pins.Version, strings.TrimSuffix(pin.File, ".tar.gz")), nil
}

// EnsureCore makes sure the pinned core of this platform is in the user cache, downloading and
// checking it when it is not, and returns its folder. Every byte is checked: the archive's size
// and sha256 against the pins this SDK was released with, then every unpacked file against the
// archive's core.json.
func EnsureCore(ctx context.Context) (string, error) {
	pins, err := loadPins()
	if err != nil {
		return "", err
	}
	root, err := cacheRoot()
	if err != nil {
		return "", err
	}
	base := os.Getenv("LATAI_CORE_URL")
	if base == "" {
		base = CoreBaseURL
	}
	return ensureCore(ctx, pins, HostTarget(), root, base)
}

func ensureCore(ctx context.Context, pins *corePins, tgt, root, base string) (string, error) {
	dir, err := coreDir(pins, root, tgt)
	if err != nil {
		return "", err
	}
	if verifyDir(dir, pins.Version, tgt) == nil {
		return dir, nil
	}
	pin := pins.Targets[tgt]
	url := strings.ReplaceAll(base, "{version}", pins.Version)
	if !strings.HasSuffix(url, "/") {
		url += "/"
	}
	url += pin.File
	archive, err := download(ctx, url, pin)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".unpack-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	top := strings.TrimSuffix(pin.File, ".tar.gz")
	if err := unpack(archive, tmp, top); err != nil {
		return "", fmt.Errorf("latai: %s: %w", pin.File, err)
	}
	if err := verifyDir(filepath.Join(tmp, top), pins.Version, tgt); err != nil {
		return "", fmt.Errorf("latai: %s: %w", pin.File, err)
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(filepath.Join(tmp, top), dir); err != nil {
		return "", err
	}
	return dir, nil
}

func download(ctx context.Context, url string, pin corePin) ([]byte, error) {
	var body io.ReadCloser
	if strings.HasPrefix(url, "file://") {
		f, err := os.Open(strings.TrimPrefix(url, "file://"))
		if err != nil {
			return nil, err
		}
		body = f
	} else {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("latai: GET %s: %s", url, resp.Status)
		}
		body = resp.Body
	}
	defer body.Close()
	limit := min(pin.Bytes, maxArchiveBytes)
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != pin.Bytes {
		return nil, fmt.Errorf("latai: %s is %d bytes, the pin says %d", url, len(data), pin.Bytes)
	}
	if got := hexSHA(data); got != pin.SHA256 {
		return nil, fmt.Errorf("latai: %s has sha256 %s, the pin says %s", url, got, pin.SHA256)
	}
	return data, nil
}

func hexSHA(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// unpack writes the regular files and folders of a tar.gz under dir; everything must be inside
// the folder top, with no absolute path, no `..`, no link and no device.
func unpack(archive []byte, dir, top string) error {
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	var total int64
	for n := 0; ; n++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if n >= maxEntries {
			return fmt.Errorf("more than %d entries", maxEntries)
		}
		name := path.Clean(h.Name)
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || (name != top && !strings.HasPrefix(name, top+"/")) {
			return fmt.Errorf("entry %q is outside %s/", h.Name, top)
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if total > maxUnpackedBytes {
				return fmt.Errorf("more than %d bytes unpacked", maxUnpackedBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, h.Size))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("entry %q is not a file or a folder", h.Name)
		}
	}
}

// verifyDir checks a core folder against its core.json: the version, the target, and every
// file's size and sha256.
func verifyDir(dir, version, tgt string) error {
	raw, err := os.ReadFile(filepath.Join(dir, "core.json"))
	if err != nil {
		return err
	}
	var m coreManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("core.json: %w", err)
	}
	if m.Version != version || m.Target != tgt {
		return fmt.Errorf("core.json is %s for %s, not %s for %s", m.Version, m.Target, version, tgt)
	}
	if len(m.Files) == 0 {
		return errors.New("core.json lists no file")
	}
	for _, f := range m.Files {
		if strings.Contains(f.Path, "..") || path.IsAbs(f.Path) {
			return fmt.Errorf("core.json names %q", f.Path)
		}
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			return err
		}
		if int64(len(data)) != f.Bytes || hexSHA(data) != f.SHA256 {
			return fmt.Errorf("%s is not the file core.json lists", f.Path)
		}
	}
	return nil
}
