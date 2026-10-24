// SPDX-License-Identifier: Apache-2.0 OR MIT

package latai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// wireDir is the WIRE-1 goldens: testdata/wire in an exported SDK, tests/fixtures/wire in latai.
func wireDir(t *testing.T) string {
	for _, d := range []string{"testdata/wire", "../../tests/fixtures/wire"} {
		if _, err := os.Stat(d); err == nil {
			return d
		}
	}
	t.Skip("no WIRE-1 goldens (testdata/wire)")
	return ""
}

// decoders by golden file prefix: each golden decodes into its Go type and back to the same JSON.
var goldenTypes = []struct {
	prefix string
	new    func() any
}{
	{"request_", func() any { return new(Request) }},
	{"response_", func() any { return new(Response) }},
	{"event_", func() any { return new(Event) }},
	{"stream_event_", func() any { return new(StreamEvent) }},
	{"stream_open", func() any { return new(StreamOpen) }},
	{"error_", func() any { return new(WireError) }},
	{"manifest", func() any { return new(Manifest) }},
	{"measured", func() any { return new(Measured) }},
	{"pack_info", func() any { return new(PackInfo) }},
	{"stage_receipt", func() any { return new(StageReceipt) }},
	{"status_", func() any { return new(Status) }},
	{"pack_versions", func() any { return new([]PackVersion) }},
}

// dropNulls removes every null object member and every empty list member: the wire reads a null,
// an empty list and an absent optional field alike (serde defaults), and Go writes none of them
// for a nil pointer or an empty slice. Where null and [] differ (`calls`), Go keeps them apart
// with a pointer: TestNullAndEmptyCallsStayApart.
func dropNulls(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			if l, ok := e.([]any); e == nil || (ok && len(l) == 0) {
				continue
			}
			out[k] = dropNulls(e)
		}
		return out
	case []any:
		for i := range x {
			x[i] = dropNulls(x[i])
		}
	}
	return v
}

func TestEveryGoldenRoundTripsThroughTheGoTypes(t *testing.T) {
	dir := wireDir(t)
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.Strings(files)
	checked := 0
	for _, f := range files {
		name := filepath.Base(f)
		var mk func() any
		for _, g := range goldenTypes {
			if strings.HasPrefix(name, g.prefix) {
				mk = g.new
				break
			}
		}
		if mk == nil {
			t.Errorf("%s: no Go type for this golden", name)
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		v := mk()
		if err := json.Unmarshal(raw, v); err != nil {
			t.Errorf("%s: decode: %v", name, err)
			continue
		}
		back, err := json.Marshal(v)
		if err != nil {
			t.Errorf("%s: encode: %v", name, err)
			continue
		}
		var want, got any
		_ = json.Unmarshal(raw, &want)
		_ = json.Unmarshal(back, &got)
		if !reflect.DeepEqual(dropNulls(want), dropNulls(got)) {
			t.Errorf("%s: round trip differs\nwant %s\ngot  %s", name, raw, back)
		}
		checked++
	}
	if checked < 40 {
		t.Errorf("only %d goldens checked", checked)
	}
}

func TestAVariantThisSDKDoesNotKnowIsKeptWhole(t *testing.T) {
	raw := `{"type":"video","frames":3,"data":"AAAA"}`
	var in Inputs
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	if in.Unknown == nil || in.Text != nil {
		t.Fatalf("%+v", in)
	}
	back, _ := json.Marshal(in)
	if string(back) != raw {
		t.Fatalf("%s", back)
	}
	if _, err := json.Marshal(Inputs{}); err == nil {
		t.Fatal("an empty union must not encode")
	}
	if err := json.Unmarshal([]byte(`{"text":"no tag"}`), &in); err == nil {
		t.Fatal("a union without its tag must not decode")
	}
}

func TestNullAndEmptyCallsStayApart(t *testing.T) {
	var none, abstained Output
	_ = json.Unmarshal([]byte(`{"kind":"calls","text":"","calls":null}`), &none)
	_ = json.Unmarshal([]byte(`{"kind":"calls","text":"","calls":[]}`), &abstained)
	if none.Calls.Calls != nil {
		t.Fatal("null calls decoded as a list")
	}
	if abstained.Calls.Calls == nil || len(*abstained.Calls.Calls) != 0 {
		t.Fatal("[] calls lost")
	}
}

func TestTheRequestHelpersWriteTheWireShape(t *testing.T) {
	b, _ := json.Marshal(ImageRequest("ocr", []byte{0x89, 'P', 'N', 'G'}, "Read the text."))
	want := `{"pack":"ocr","inputs":{"type":"image","data":"iVBORw==","prompt":"Read the text."}}`
	if string(b) != want {
		t.Fatalf("%s", b)
	}
	b, _ = json.Marshal(TextRequest("p", "hi"))
	if string(b) != `{"pack":"p","inputs":{"type":"text","text":"hi"}}` {
		t.Fatalf("%s", b)
	}
}

func TestErrorsKnowTheirNameCategoryAndRetry(t *testing.T) {
	e := wireError(206, []byte(`{"code":"E0206","message":"m","hint":"h","details":{"limit":"params"}}`))
	if e.Name() != "CeilingExceeded" || e.Category() != "compat" || e.Retryable() {
		t.Fatalf("%v %v %v", e.Name(), e.Category(), e.Retryable())
	}
	if !HasCode(e, CodeCeilingExceeded) {
		t.Fatal("HasCode")
	}
	// A call that failed with no error JSON still has its code and the table's hint.
	e = wireError(316, nil)
	if e.Code != "E0316" || e.Hint == "" {
		t.Fatalf("%+v", e)
	}
	if (&Error{Code: "E0999"}).Name() != "" {
		t.Fatal("an unknown code has no name")
	}
}

// In a latai checkout, the module's copy of the C header is the header of the core.
func TestTheVendoredHeaderIsTheCoresHeader(t *testing.T) {
	for _, name := range []string{"latai.h", "latai_errors.h"} {
		want, err := os.ReadFile(filepath.Join("..", "..", "include", name))
		if err != nil {
			t.Skip("not in a latai checkout")
		}
		got, _ := os.ReadFile(filepath.Join("include", name))
		if string(got) != string(want) {
			t.Errorf("include/%s differs from latai's include/%s: copy it again", name, name)
		}
	}
}
