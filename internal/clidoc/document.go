// Package clidoc translates bounded, opaque JSON/YAML objects without losing
// numeric precision or interpreting DSC fields.
package clidoc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Bpoe/dsc-registry/internal/desired"
	"go.yaml.in/yaml/v3"
)

const maxDocumentBytes = 32 << 20
const maxDepth = 128
const maxNodes = 1 << 20

var errDocument = errors.New("invalid document: expected bounded JSON/YAML objects with unique string keys and JSON-compatible values")
var decimal = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)
var yamlInteger = regexp.MustCompile(`^[+-]?(0[xX][0-9a-fA-F_]+|0[oO][0-7_]+|0[bB][01_]+|[0-9][0-9_]*)$`)
var yamlFloat = regexp.MustCompile(`^[+-]?([0-9][0-9_]*(\.[0-9_]*)?|\.[0-9_]+)([eE][+-]?[0-9]+)?$`)
var streamMarker = regexp.MustCompile(`(?m)^---(?:[ \t]*(?:#.*)?)?\r?$`)
var jsonObjectStart = regexp.MustCompile(`^\{\s*(?:"|})`)

func Decode(data []byte) ([]map[string]any, error) {
	if len(data) > maxDocumentBytes || !utf8.Valid(data) {
		return nil, errDocument
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errDocument
	}
	if (jsonObjectStart.Match(trimmed) || trimmed[0] == '[') && !streamMarker.Match(data) {
		value, err := jsonValue(data)
		if err != nil {
			return nil, err
		}
		object, ok := value.(map[string]any)
		if !ok {
			return nil, errDocument
		}
		return []map[string]any{object}, nil
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	documents := make([]map[string]any, 0)
	budget := maxNodes
	sizeBudget := maxDocumentBytes
	for {
		var node yaml.Node
		if err := decoder.Decode(&node); err == io.EOF {
			break
		} else if err != nil {
			return nil, errDocument
		}
		if len(node.Content) != 1 {
			return nil, errDocument
		}
		value, err := yamlValue(node.Content[0], 0, map[*yaml.Node]bool{}, &budget, &sizeBudget)
		if err != nil {
			return nil, err
		}
		object, ok := value.(map[string]any)
		if !ok {
			return nil, errDocument
		}
		documents = append(documents, object)
	}
	if len(documents) == 0 {
		return nil, errDocument
	}
	encoded, err := json.Marshal(documents)
	if err != nil || len(encoded) > maxDocumentBytes {
		return nil, errDocument
	}
	return documents, nil
}

func jsonValue(data []byte) (any, error) {
	if desired.ValidateJSON(data) != nil {
		return nil, errDocument
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, errDocument
	}
	return value, nil
}

func yamlValue(node *yaml.Node, depth int, active map[*yaml.Node]bool, budget, sizeBudget *int) (any, error) {
	*budget--
	if depth > maxDepth || *budget < 0 || node == nil || active[node] {
		return nil, errDocument
	}
	// Charge each alias expansion, not just each input node, before allocating
	// the expanded JSON. This bounds aliases referencing large scalar values.
	*sizeBudget -= len(node.Value) + 2
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		encoded, _ := json.Marshal(node.Value)
		*sizeBudget -= len(encoded) - len(node.Value)
	}
	if *sizeBudget < 0 {
		return nil, errDocument
	}
	active[node] = true
	defer delete(active, node)
	switch node.Kind {
	case yaml.AliasNode:
		return yamlValue(node.Alias, depth+1, active, budget, sizeBudget)
	case yaml.MappingNode:
		if node.Tag != "!!map" || len(node.Content)%2 != 0 {
			return nil, errDocument
		}
		result := make(map[string]any)
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return nil, errDocument
			}
			keyValue, err := yamlValue(key, depth+1, active, budget, sizeBudget)
			if err != nil {
				return nil, err
			}
			name, ok := keyValue.(string)
			if !ok {
				return nil, errDocument
			}
			if _, exists := result[name]; exists {
				return nil, errDocument
			}
			value, err := yamlValue(node.Content[i+1], depth+1, active, budget, sizeBudget)
			if err != nil {
				return nil, err
			}
			result[name] = value
		}
		return result, nil
	case yaml.SequenceNode:
		if node.Tag != "!!seq" {
			return nil, errDocument
		}
		result := make([]any, 0, len(node.Content))
		for _, child := range node.Content {
			value, err := yamlValue(child, depth+1, active, budget, sizeBudget)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		return result, nil
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!str":
			// yaml's resolver labels exponent overflow as a string. Plain JSON
			// numbers are still exact numbers, irrespective of float64 range.
			if node.Style == 0 && decimal.MatchString(node.Value) {
				return json.Number(node.Value), nil
			}
			if node.Style == 0 && yamlInteger.MatchString(node.Value) {
				return yamlNumber(node.Value, "!!int")
			}
			if node.Style == 0 && yamlFloat.MatchString(node.Value) {
				return yamlNumber(node.Value, "!!float")
			}
			return node.Value, nil
		case "!!null":
			switch strings.ToLower(node.Value) {
			case "", "null", "~":
				return nil, nil
			}
		case "!!bool":
			switch strings.ToLower(node.Value) {
			case "true":
				return true, nil
			case "false":
				return false, nil
			}
		case "!!int", "!!float":
			return yamlNumber(node.Value, node.Tag)
		}
	}
	return nil, errDocument
}

func yamlNumber(value, tag string) (json.Number, error) {
	if tag == "!!int" && !yamlInteger.MatchString(value) || tag == "!!float" && !yamlFloat.MatchString(value) {
		return "", errDocument
	}
	value = strings.ReplaceAll(value, "_", "")
	value = strings.TrimPrefix(value, "+")
	if decimal.MatchString(value) {
		return json.Number(value), nil
	}
	if tag == "!!int" {
		n := new(big.Int)
		if _, ok := n.SetString(value, 0); ok {
			return json.Number(n.String()), nil
		}
	}
	if tag == "!!float" {
		if strings.HasPrefix(value, ".") {
			value = "0" + value
		} else if strings.HasPrefix(value, "-.") {
			value = "-0" + value[1:]
		}
		mantissa, exponent, found := strings.Cut(strings.ToLower(value), "e")
		if strings.HasSuffix(mantissa, ".") {
			mantissa += "0"
		}
		sign := ""
		if strings.HasPrefix(mantissa, "-") {
			sign, mantissa = "-", mantissa[1:]
		}
		integer, fraction, fractional := strings.Cut(mantissa, ".")
		integer = strings.TrimLeft(integer, "0")
		if integer == "" {
			integer = "0"
		}
		mantissa = sign + integer
		if fractional {
			mantissa += "." + fraction
		}
		value = mantissa
		if found {
			value += "e" + exponent
		}
		if decimal.MatchString(value) {
			return json.Number(value), nil
		}
	}
	return "", errDocument
}

func ObjectFile(path string, in io.Reader) (map[string]any, error) {
	var reader io.Reader = in
	if path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return nil, errors.New("cannot open document file")
		}
		defer file.Close()
		reader = file
	}
	if reader == nil {
		return nil, errors.New("document input is unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxDocumentBytes+1))
	if err != nil {
		return nil, errors.New("cannot read document")
	}
	documents, err := Decode(data)
	if err != nil {
		return nil, err
	}
	if len(documents) != 1 {
		return nil, errors.New("expected exactly one document object")
	}
	return documents[0], nil
}

func Encode(value any, format string) ([]byte, error) {
	if format != "json" && format != "yaml" {
		return nil, errors.New("document format must be json or yaml")
	}
	data, err := json.Marshal(value)
	if err != nil || len(data) > maxDocumentBytes {
		return nil, errDocument
	}
	normalized, err := jsonValue(data)
	if err != nil {
		return nil, err
	}
	if format == "json" {
		var output bytes.Buffer
		if err := json.Indent(&output, data, "", "  "); err != nil {
			return nil, errDocument
		}
		output.WriteByte('\n')
		return output.Bytes(), nil
	}
	node := yamlNode(normalized)
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(node); err != nil {
		return nil, errDocument
	}
	if err := encoder.Close(); err != nil {
		return nil, errDocument
	}
	return output.Bytes(), nil
}

func yamlNode(value any) *yaml.Node {
	switch value := value.(type) {
	case map[string]any:
		node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, yamlNode(value[key]))
		}
		return node
	case []any:
		node := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, item := range value {
			node.Content = append(node.Content, yamlNode(item))
		}
		return node
	case json.Number:
		tag := "!!int"
		if strings.ContainsAny(string(value), ".eE") {
			tag = "!!float"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: string(value)}
	case string:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	case bool:
		text := "false"
		if value {
			text = "true"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: text}
	default:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	}
}
