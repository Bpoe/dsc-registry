package desired

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCanonicalJSON(t *testing.T) {
	first, err := Parse([]byte(`{
		"parameters":{"empty":{}, "array":[]},
		"template":{"z":9007199254740993123456789, "a":{"unknown":[true,null,"<>&",1.20,1e999999]},
		"metadata":{"dscd":{"operation":"anything"}},"unicode":"\uD83D\uDE00","nul":"\u0000"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Parse([]byte(`{"template":{"nul":"\u0000","unicode":"😀","metadata":{"dscd":{"operation":"anything"}},
		"a":{"unknown":[true,null,"\u003c\u003e\u0026",12e-1,10e999998]},"z":9007199254740993123456789.000},
		"parameters":{"array":[],"empty":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Template, second.Template) || !bytes.Equal(first.Parameters, second.Parameters) {
		t.Fatalf("canonical forms differ:\n%s\n%s", first.Template, second.Template)
	}
	if !bytes.Contains(first.Template, []byte("9007199254740993123456789")) ||
		!bytes.Contains(first.Template, []byte("1e999999")) {
		t.Fatal("numeric precision/range lost")
	}
	for _, pair := range [][2]string{
		{"1", "1.00e0"}, {"1000", "10e2"}, {"-0.000e-9999", "0"},
		{"0.00120", "120e-5"}, {"-123000.000", "-123e3"},
		{"1e-000123", "10e-124"}, {"1e" + strings.Repeat("9", 1000), "1e+" + strings.Repeat("9", 1000)},
	} {
		left, err := Parse([]byte(`{"template":{"n":` + pair[0] + `}}`))
		if err != nil {
			t.Fatal(err)
		}
		right, err := Parse([]byte(`{"template":{"n":` + pair[1] + `}}`))
		if err != nil || !bytes.Equal(left.Template, right.Template) {
			t.Fatalf("number normalization %v: %s != %s (%v)", pair, left.Template, right.Template, err)
		}
	}
	for _, body := range []string{`{"template":{}}`, `{"template":{},"parameters":null}`} {
		content, err := Parse([]byte(body))
		if err != nil || content.Parameters != nil {
			t.Fatal("missing/null parameters must be absent")
		}
	}
	content, err := Parse([]byte(`{"template":{},"parameters":{}}`))
	if err != nil || string(content.Parameters) != "{}" {
		t.Fatal("present empty parameters lost")
	}
}

func TestInvalidJSON(t *testing.T) {
	for _, body := range []string{
		``, `{`, `null`, `[]`, `"document"`, `template: {}`, `{}`, `{"template":null}`,
		`{"template":[]}`, `{"template":true}`, `{"template":1}`, `{"template":"{}"}`,
		`{"template":{},"parameters":[]}`, `{"template":{},"parameters":false}`,
		`{"template":{},"parameters":"{}"}`, `{"template":{},"parameters":1}`,
		`{"Template":{}}`, `{"template":{},"unknown":null}`,
		`{"template":{},"template":{}}`, `{"template":{},"tem\u0070late":{}}`,
		`{"template":{"a":1,"a":2}}`, `{"template":{"a":[{"b":1,"b":2}]}}`,
		`{"template":{},"parameters":{"x":1,"x":2}}`,
		`{"template":{"format":"json","content":"{}"}}`,
		`{"template":{},"parameters":{"format":"yaml","content":"x: 1"}}`,
		`{"template":{"s":"\ud800"}}`, `{"template":{"s":"\udfff"}}`,
		`{"template":{"s":"\ud800\u0041"}}`, `{"template":{"s":"\ud800\\udfff"}}`,
		"{\"template\":{\"s\":\"\xff\"}}", `{"template":{}} {}`, `{"template":{},}`,
		`{"template":{"n":01}}`, `{"template":{"n":NaN}}`,
		`{"template":{"a":` + strings.Repeat("[", 129) + "0" + strings.Repeat("]", 129) + `}}`,
	} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("accepted invalid body: %.160s", body)
		}
	}
	for _, body := range []string{
		`{"template":{"s":"\\ud800"}}`, `{"template":{"s":"\ud800\udc00"}}`,
		`{"template":{"content":{"unknown":true}}}`, `{"template":{"format":"unknown"}}`,
	} {
		if _, err := Parse([]byte(body)); err != nil {
			t.Errorf("rejected opaque valid content %s: %v", body, err)
		}
	}
}

func TestConfigurationNames(t *testing.T) {
	for _, name := range []string{"a", "0", "a.b_c-d", strings.Repeat("a", 64), "parameters", "x.parameters-x"} {
		if !ValidName(name) {
			t.Errorf("rejected name %q", name)
		}
	}
	for _, name := range []string{"", ".hidden", "-name", "_name", "UPPER", "space name", "x.", "x.parameters", "x/y", `x\y`, "é", "a\x00", strings.Repeat("a", 65)} {
		if ValidName(name) {
			t.Errorf("accepted name %q", name)
		}
	}
}

func FuzzCanonicalJSON(f *testing.F) {
	for _, seed := range []string{
		`{"template":{}}`, `{"template":{"n":1.0,"array":[null,{},[]]},"parameters":{}}`,
		`{"template":{"s":"\ud800"}}`, `{"template":{"n":1e999999}}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		content, err := Parse(body)
		if err != nil {
			return
		}
		encoded, err := json.Marshal(content)
		if err != nil {
			t.Fatal(err)
		}
		again, err := Parse(encoded)
		if err != nil || !bytes.Equal(content.Template, again.Template) || !bytes.Equal(content.Parameters, again.Parameters) {
			t.Fatalf("canonical round-trip changed accepted content: %v", err)
		}
	})
}
