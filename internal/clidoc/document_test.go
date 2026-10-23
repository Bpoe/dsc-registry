package clidoc

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDecodePrecisionAndOpaqueFields(t *testing.T) {
	for _, input := range []string{
		`{"huge":1234567890123456789012345678901234567890,"fraction":1.234567890123456789,"exponent":1e9999,"parameters":null,"future":{"list":[false,{},null],"operation":"test"}}`,
		"huge: 1234567890123456789012345678901234567890\nfraction: 1.234567890123456789\nexponent: 1e9999\nparameters: null\nfuture:\n  list: [false, {}, null]\n  operation: test\n",
	} {
		documents, err := Decode([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		object := documents[0]
		for key, expected := range map[string]json.Number{
			"huge": "1234567890123456789012345678901234567890", "fraction": "1.234567890123456789", "exponent": "1e9999",
		} {
			if object[key] != expected {
				t.Errorf("%s: %#v, want %#v", key, object[key], expected)
			}
		}
		for _, format := range []string{"json", "yaml"} {
			encoded, err := Encode(object, format)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := Decode(encoded)
			if err != nil || !reflect.DeepEqual(decoded[0], object) {
				t.Fatalf("%s round trip: %s, %#v, %v", format, encoded, decoded, err)
			}
		}
	}
}

func TestDecodeRejectsAmbiguousOrNonObjectInput(t *testing.T) {
	for _, input := range []string{
		"", " ", "null", "[]", "42", "hello", `{"a":1,"a":2}`,
		`{"a":{"x":1,"x":2}}`, `{"a":1} {"b":2}`, `{"a":1} trailing`,
		"{\"a\":\"\\ud800\"}", "{\"a\":\"\\udc00\"}", "{\"a\":\"\xff\"}",
		"a: 1\na: 2\n", "outer:\n  a: 1\n  a: 2\n", "1: value\n",
		"? [a,b]\n: value\n", "a: !unknown value\n", "a: !!binary aGVsbG8=\n",
		"a: .inf\n", "a: .NaN\n", "a: 2026-10-23\n",
		"a: !!null not-null\n", "a: !!float not-a-number\n", "a: !!int not-an-int\n",
		"1234567890123456789012345678901234567890: value\n",
		"a: &a [*a]\n", "a: &a {b: *a}\n", "a: *missing\n",
		"a: &base {key: value}\nb: {<<: *base}\n",
		"a: ok\n---\n[]\n", "a: ok\n---\n", "a: " + strings.Repeat("[", 130) + "0" + strings.Repeat("]", 130),
		`{"a":` + strings.Repeat("[", 130) + "0" + strings.Repeat("]", 130) + "}",
	} {
		t.Run(input[:min(len(input), 40)], func(t *testing.T) {
			if _, err := Decode([]byte(input)); err == nil {
				t.Fatalf("accepted %q", input)
			}
		})
	}
}

func TestYAMLDocumentsAndAliases(t *testing.T) {
	for _, input := range []string{
		"---\na: &x {nested: [1, null]}\nb: *x\n---\nc: true\n",
		"{\"a\": 1}\n---\n{\"c\": true}\n",
		"{a: 1, b: 'opaque'}\n---\nc: true\n",
	} {
		documents, err := Decode([]byte(input))
		if err != nil || len(documents) != 2 {
			t.Fatalf("%q: %#v, %v", input, documents, err)
		}
	}
}

func TestYAMLNumericSpellings(t *testing.T) {
	documents, err := Decode([]byte("hex: 0xFFFFFFFFFFFFFFFFFFFFFFFF\nbinary: 0b101\nsigned: +12\nhalf: .5\nnegative: -.5\npoint: 1.\nleading: 09\nleadingFraction: 001.2\nquoted: '1e9999'\nstring: !!str 1e9999\n"))
	if err != nil {
		t.Fatal(err)
	}
	for key, expected := range map[string]any{
		"hex": json.Number("79228162514264337593543950335"), "binary": json.Number("5"),
		"signed": json.Number("12"), "half": json.Number("0.5"), "negative": json.Number("-0.5"),
		"point": json.Number("1.0"), "quoted": "1e9999", "string": "1e9999",
		"leading": json.Number("9"), "leadingFraction": json.Number("1.2"),
	} {
		if documents[0][key] != expected {
			t.Errorf("%s = %#v, want %#v", key, documents[0][key], expected)
		}
	}
}

func TestObjectFileAndSizeBounds(t *testing.T) {
	value, err := ObjectFile("-", strings.NewReader("template: {}\nparameters: {}\n"))
	if err != nil || value["parameters"] == nil {
		t.Fatalf("%#v, %v", value, err)
	}
	if _, err := ObjectFile("-", strings.NewReader("a: 1\n---\nb: 2")); err == nil {
		t.Fatal("accepted multiple objects")
	}
	if _, err := ObjectFile("-", nil); err == nil {
		t.Fatal("accepted missing stdin")
	}
	// Keep scratch files inside the checkout, including on Windows.
	file, err := os.CreateTemp(".", "document-test-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString("template: {}\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ObjectFile(filepath.Clean(file.Name()), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ObjectFile("does-not-exist", nil); err == nil {
		t.Fatal("accepted nonexistent file")
	}
	if _, err := Decode(bytes.Repeat([]byte(" "), maxDocumentBytes+1)); err == nil {
		t.Fatal("accepted oversized input")
	}
	largeAlias := "a: &large '" + strings.Repeat("x", 1<<20) + "'\nb: [" + strings.Repeat("*large,", 40) + "]"
	if _, err := Decode([]byte(largeAlias)); err == nil {
		t.Fatal("accepted oversized alias expansion")
	}
}

func TestEncodeTypedAndRaw(t *testing.T) {
	type response struct {
		Count json.Number     `json:"count"`
		Raw   json.RawMessage `json:"opaque"`
	}
	for _, format := range []string{"json", "yaml"} {
		encoded, err := Encode(response{Count: "9007199254740993", Raw: json.RawMessage(`{"more":[null,{}],"tiny":1e-9999}`)}, format)
		if err != nil {
			t.Fatal(err)
		}
		documents, err := Decode(encoded)
		if err != nil || documents[0]["count"] != json.Number("9007199254740993") {
			t.Fatalf("%s: %s, %#v, %v", format, encoded, documents, err)
		}
	}
	for _, value := range []any{json.RawMessage(`{"a":1,"a":2}`), json.RawMessage(`{"s":"\ud800"}`), json.Number("bad"), make(chan int)} {
		if _, err := Encode(value, "yaml"); err == nil {
			t.Fatalf("accepted %#v", value)
		}
	}
	if _, err := Encode(map[string]any{}, "table"); err == nil {
		t.Fatal("accepted invalid format")
	}
}
