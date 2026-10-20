package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Fixtures mirror dsc-reconciler cce7df1764c2f25fe5e1d1066a77e387d2b218b5,
// internal/dsc/{result,client,protocol}.go and internal/reconcile/reconciler.go.
const success = `{
  "schemaVersion":1,"configuration":"managed/docker.json","operation":"test",
  "parameters":"managed/docker.parameters.json",
  "inputHash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "startedAt":"2026-10-18T12:00:00Z","finishedAt":"2026-10-18T12:00:00.005Z",
  "durationMs":5,"outcome":"succeeded","exitCode":null,
  "dscResult":{"metadata":{},"results":[{"result":{"inDesiredState":false,"actualState":{"large":9007199254740993123456789,"huge":1e999999}}}],"messages":[],"hadErrors":false},
  "error":null,"stderr":"","future":{"opaque":"\u0000"}
}`

func wrapped(result string) []byte {
	return []byte(`{"configurationRevision":"11111111-1111-4111-8111-111111111111","uploadId":"22222222-2222-4222-8222-222222222222","result":` + result + `}`)
}

func TestRealEnvelopes(t *testing.T) {
	got, err := Parse(wrapped(success), "docker")
	if err != nil || got.Outcome != "succeeded" || got.Operation == nil || *got.Operation != "test" ||
		got.ErrorKind != nil || !bytes.Equal(got.Result, []byte(success)) {
		t.Fatalf("successful test with drift not preserved: %+v %v", got, err)
	}
	for _, kind := range []string{"input", "start", "dsc", "output", "exit", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			for _, optional := range []bool{false, true} {
				var envelope map[string]json.RawMessage
				if err := json.Unmarshal([]byte(success), &envelope); err != nil {
					t.Fatal(err)
				}
				envelope["outcome"] = json.RawMessage(`"failed"`)
				if kind == "canceled" {
					envelope["outcome"] = json.RawMessage(`"canceled"`)
				}
				envelope["error"] = json.RawMessage(`{"kind":"` + kind + `","message":"sensitive diagnostic"}`)
				envelope["operation"] = json.RawMessage(`null`)
				envelope["dscResult"] = json.RawMessage(`null`)
				if kind == "exit" {
					envelope["exitCode"] = json.RawMessage(`23`)
				}
				if optional {
					for _, field := range []string{"operation", "parameters", "inputHash", "exitCode", "dscResult", "stderr"} {
						delete(envelope, field)
					}
				}
				body, err := json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
				got, err := Parse(wrapped(string(body)), "docker")
				if err != nil || got.ErrorKind == nil || *got.ErrorKind != kind {
					t.Fatalf("legal failure rejected (omitted=%v): %v", optional, err)
				}
			}
		})
	}
}

func TestNamedUpload(t *testing.T) {
	body := append([]byte(`{"configurationName":"docker",`), wrapped(success)[1:]...)
	got, err := Parse(body, "")
	if err != nil || got.Name != "docker" || !bytes.Equal(got.Result, []byte(success)) {
		t.Fatal("named upload changed original envelope", err)
	}
	got, err = Parse(bytes.Replace(body, []byte(`"configurationName":"docker"`), []byte(`"configurationName":"other"`), 1), "")
	if err != nil || got.Name != "other" || !bytes.Equal(got.Result, []byte(success)) {
		t.Fatal("embedded path overrode configuration name", err)
	}
	for _, invalid := range []string{
		string(wrapped(success)),
		strings.Replace(string(body), `"configurationName":"docker"`, `"configurationName":null`, 1),
		strings.Replace(string(body), `"configurationName"`, `"ConfigurationName"`, 1),
		strings.Replace(string(body), `"configurationName":"docker"`, `"configurationName":"../docker"`, 1),
		strings.Replace(string(body), `"configurationName":"docker"`, `"configurationName":"docker","configurationName":"docker"`, 1),
		strings.Replace(string(body), `"configurationName"`, `"machineId":"untrusted","configurationName"`, 1),
	} {
		if _, err := Parse([]byte(invalid), ""); err == nil {
			t.Fatal("invalid named upload accepted")
		}
	}
	if _, err := Parse(body, "docker"); err == nil {
		t.Fatal("legacy path accepted a configuration override")
	}
}

func TestReportPathFields(t *testing.T) {
	for _, field := range []struct {
		name, original string
		optional       bool
	}{
		{"configuration", `"managed/docker.json"`, false},
		{"parameters", `"managed/docker.parameters.json"`, true},
	} {
		for _, value := range []struct {
			name, raw string
		}{
			{"missing", ""},
			{"empty", `""`},
			{"null", `null`},
			{"number", `7`},
			{"boolean", `true`},
			{"array", `[]`},
			{"object", `{}`},
		} {
			t.Run(field.name+"/"+value.name, func(t *testing.T) {
				replacement := ""
				if value.raw != "" {
					replacement = `"` + field.name + `":` + value.raw + ","
				}
				result := strings.Replace(success, `"`+field.name+`":`+field.original+",", replacement, 1)
				got, err := Parse(wrapped(result), "docker")
				valid := field.optional && (value.raw == "" || value.raw == `""` || value.raw == "null")
				if valid {
					if err != nil || !bytes.Equal(got.Result, []byte(result)) {
						t.Fatal("optional parameter value rejected or changed", err)
					}
				} else if err == nil {
					t.Fatal("invalid path field accepted")
				}
			})
		}
	}
}

func FuzzReportEnvelope(f *testing.F) {
	f.Add(wrapped(success))
	f.Add([]byte(`{}`))
	f.Add(wrapped(strings.Replace(success, `"schemaVersion":1`, `"schemaVersion":1,"SchemaVersion":2`, 1)))
	f.Fuzz(func(t *testing.T, body []byte) {
		upload, err := Parse(body, "docker")
		if err != nil {
			return
		}
		var wrapper map[string]json.RawMessage
		if err := json.Unmarshal(body, &wrapper); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(upload.Result, bytes.TrimSpace(wrapper["result"])) {
			t.Fatal("accepted report bytes changed")
		}
		again, err := Parse(wrapped(string(upload.Result)), "docker")
		if err != nil || !bytes.Equal(again.Result, upload.Result) || again.Outcome != upload.Outcome {
			t.Fatal("accepted result did not survive rewrapping")
		}
	})
}

func TestInvalidEnvelopes(t *testing.T) {
	for name, replacement := range map[string]string{
		"schemaVersion": `2`, "configuration": `""`, "operation": `"get"`,
		"parameters": `[]`, "inputHash": `"not-the-server-revision"`,
		"startedAt": `null`, "finishedAt": `"2026-10-18T11:00:00Z"`, "durationMs": `-1`,
		"outcome": `"compliant"`, "exitCode": `"1"`, "dscResult": `[]`, "error": `{"kind":"unknown","message":""}`, "stderr": `7`,
	} {
		t.Run(name, func(t *testing.T) {
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal([]byte(success), &envelope); err != nil {
				t.Fatal(err)
			}

			envelope[name] = json.RawMessage(replacement)
			data, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(wrapped(string(data)), "docker"); err == nil {
				t.Fatal("invalid field accepted")
			}
			delete(envelope, name)
			data, _ = json.Marshal(envelope)
			switch name {
			case "schemaVersion", "configuration", "startedAt", "finishedAt", "durationMs", "outcome", "operation", "dscResult":
				if _, err := Parse(wrapped(string(data)), "docker"); err == nil {
					t.Fatal("missing required success field accepted")
				}
			}
		})
	}
	for _, data := range [][]byte{
		[]byte(`null`), []byte(`[]`), []byte(`{}`),
		[]byte(strings.Replace(string(wrapped(success)), `"uploadId":`, `"unknown":`, 1)),
		[]byte(strings.Replace(string(wrapped(success)), `"uploadId":`, `"uploadId":null,"uploadId":`, 1)),
		[]byte(strings.Replace(string(wrapped(success)), `"schemaVersion":1`, `"schemaVersion":1,"schema\u0056ersion":1`, 1)),
		[]byte(strings.Replace(string(wrapped(success)), `"schemaVersion":1`, `"schemaVersion":1,"SchemaVersion":2`, 1)),
		[]byte(strings.Replace(string(wrapped(success)), `"error":null`, `"error":null,"Error":{"kind":"dsc","message":"alias"}`, 1)),
		[]byte(strings.Replace(string(wrapped(success)), `"opaque":"\u0000"`, `"opaque":"\ud800"`, 1)),
		[]byte(strings.Replace(string(wrapped(success)), `"opaque":"\u0000"`, `"opaque":`+strings.Repeat("[", 130)+`0`+strings.Repeat("]", 130), 1)),
		append(wrapped(success), []byte(`{}`)...),
		bytes.Replace(wrapped(success), []byte("opaque"), []byte{0xff}, 1),
		[]byte(strings.Replace(string(wrapped(success)), "22222222-2222-4222-8222-222222222222", "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", 1)),
	} {
		if _, err := Parse(data, "docker"); err == nil {
			t.Fatalf("ambiguous or malformed input accepted: %s", data)
		}
	}
}
