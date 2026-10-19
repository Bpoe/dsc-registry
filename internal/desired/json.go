// Package desired validates opaque desired-state JSON without interpreting DSC.
package desired

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"strings"
	"unicode/utf8"
)

var ErrInvalid = errors.New("invalid desired configuration JSON")

type Content struct {
	Template   json.RawMessage `json:"template"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

func ValidName(name string) bool {
	if len(name) == 0 || len(name) > 64 || name[len(name)-1] == '.' ||
		strings.HasSuffix(name, ".parameters") {
		return false
	}
	for i, c := range []byte(name) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			continue
		}
		if i == 0 || c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func Parse(body []byte) (Content, error) {
	if !utf8.Valid(body) || !validSurrogates(body) {
		return Content{}, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	value, err := readValue(d, 0)
	if err != nil {
		return Content{}, ErrInvalid
	}
	if _, err := d.Token(); err != io.EOF {
		return Content{}, ErrInvalid
	}
	object, ok := value.(map[string]any)
	if !ok {
		return Content{}, ErrInvalid
	}
	for key := range object {
		if key != "template" && key != "parameters" {
			return Content{}, ErrInvalid
		}
	}
	template, ok := document(object["template"])
	if !ok {
		return Content{}, ErrInvalid
	}
	var parameters json.RawMessage
	if object["parameters"] != nil {
		parameters, ok = document(object["parameters"])
		if !ok {
			return Content{}, ErrInvalid
		}
	}
	return Content{Template: template, Parameters: parameters}, nil
}

func document(value any) (json.RawMessage, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	// Reject the legacy document envelope, not opaque DSC fields named content.
	if _, format := object["format"]; format {
		if _, content := object["content"]; content {
			return nil, false
		}
	}
	encoded, err := json.Marshal(object)
	return encoded, err == nil
}

func readValue(d *json.Decoder, depth int) (any, error) {
	if depth > 128 {
		return nil, ErrInvalid
	}
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		object := make(map[string]any)
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, ErrInvalid
			}
			if _, duplicate := object[name]; duplicate {
				return nil, ErrInvalid
			}
			value, err := readValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		if end, err := d.Token(); err != nil || end != json.Delim('}') {
			return nil, ErrInvalid
		}
		return object, nil
	case json.Delim('['):
		array := make([]any, 0)
		for d.More() {
			value, err := readValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if end, err := d.Token(); err != nil || end != json.Delim(']') {
			return nil, ErrInvalid
		}
		return array, nil
	default:
		switch value := token.(type) {
		case json.Number:
			return canonicalNumber(string(value)), nil
		case string, bool, nil:
			return value, nil
		default:
			return nil, ErrInvalid
		}
	}
}

// Normalize decimal coefficients/exponents exactly, without floats or expanding
// large exponents. Numerically equal spellings have the same representation.
func canonicalNumber(value string) json.Number {
	negative := strings.HasPrefix(value, "-")
	value = strings.TrimPrefix(value, "-")
	mantissa, exponent, _ := strings.Cut(strings.ToLower(value), "e")
	power := new(big.Int)
	if exponent != "" {
		power.SetString(strings.TrimPrefix(exponent, "+"), 10)
	}
	if dot := strings.IndexByte(mantissa, '.'); dot >= 0 {
		power.Sub(power, big.NewInt(int64(len(mantissa)-dot-1)))
		mantissa = mantissa[:dot] + mantissa[dot+1:]
	}
	mantissa = strings.TrimLeft(mantissa, "0")
	if mantissa == "" {
		return "0"
	}
	trimmed := strings.TrimRight(mantissa, "0")
	power.Add(power, big.NewInt(int64(len(mantissa)-len(trimmed))))
	if negative {
		trimmed = "-" + trimmed
	}
	if power.Sign() != 0 {
		trimmed += "e" + power.String()
	}
	return json.Number(trimmed)
}

// encoding/json replaces unpaired UTF-16 escapes with U+FFFD. Reject them
// instead, so accepted opaque strings are never silently changed.
func validSurrogates(body []byte) bool {
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			continue
		}
		i++
		if i >= len(body) || body[i] != 'u' {
			continue
		}
		code, ok := hexQuad(body[i+1:])
		if !ok {
			return false
		}
		i += 4
		if code >= 0xdc00 && code <= 0xdfff {
			return false
		}
		if code >= 0xd800 && code <= 0xdbff {
			if i+6 >= len(body) || body[i+1] != '\\' || body[i+2] != 'u' {
				return false
			}
			low, ok := hexQuad(body[i+3:])
			if !ok || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}

func hexQuad(value []byte) (uint16, bool) {
	if len(value) < 4 {
		return 0, false
	}
	var code uint16
	for _, c := range value[:4] {
		code <<= 4
		switch {
		case c >= '0' && c <= '9':
			code += uint16(c - '0')
		case c >= 'a' && c <= 'f':
			code += uint16(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			code += uint16(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	return code, true
}
