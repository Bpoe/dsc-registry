package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bpoe/dsc-registry/internal/clidoc"
)

const resourceMachineID = "550e8400-e29b-41d4-a716-446655440000"
const resourceReportID = "2e631f22-1183-4264-b131-a328f7dfe789"

func resourceFixture(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	directory := t.TempDir()
	for _, variable := range []string{"DSCREG_CONTEXT", "DSCREG_SERVER", "DSCREG_API_KEY_FILE"} {
		t.Setenv(variable, "")
	}
	t.Setenv("DSCREG_CONFIG", filepath.Join(directory, "config.yaml"))
	t.Setenv("DSCREG_API_KEY", "dsca_test.secret")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Setenv("DSCREG_SERVER", server.URL)
	return directory
}

func resourceRun(input string, args ...string) (int, string, string) {
	var out, diagnostic bytes.Buffer
	code := Execute(context.Background(), args, strings.NewReader(input), &out, &diagnostic)
	return code, out.String(), diagnostic.String()
}

func resourceJSON(w http.ResponseWriter, value string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, value)
}

func fixtureMachine(w http.ResponseWriter) {
	resourceJSON(w, `{"id":"`+resourceMachineID+`","name":"apphost01","configurationRevision":7}`)
}

func TestResourceGetAliasesPaginationAndLosslessOutput(t *testing.T) {
	var paths []string
	resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.Header.Get("Authorization") != "ApiKey dsca_test.secret" {
			t.Error("administrator authorization not sent")
		}
		if r.URL.Query().Has("name") || r.URL.Path == "/v1/machines/"+resourceMachineID {
			fixtureMachine(w)
			return
		}
		resourceJSON(w, `{"machines":[{"id":"`+resourceMachineID+`","name":"apphost01","configurationRevision":9007199254740993,"future":null}],"nextCursor":"`+resourceMachineID+`","unknown":{"n":99999999999999999999}}`)
	})
	code, output, diagnostic := resourceRun("", "get", "machines", "-o", "json", "--limit", "2")
	if code != 0 || !strings.Contains(output, "9007199254740993") || !strings.Contains(output, "99999999999999999999") || !strings.Contains(output, `"nextCursor"`) {
		t.Fatalf("lossless output: %d %s %s", code, output, diagnostic)
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(output), &object); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"machine", "mc", "machines"} {
		code, output, diagnostic = resourceRun("", "get", alias, "AppHost01", "-o", "name")
		if code != 0 || output != "machine/apphost01\n" {
			t.Fatalf("%s: %d %q %s", alias, code, output, diagnostic)
		}
	}
	if !strings.Contains(paths[0], "limit=2") || !strings.Contains(paths[1], "name=AppHost01") {
		t.Fatalf("requests %v", paths)
	}
	code, output, diagnostic = resourceRun("", "get", "machine", resourceMachineID, "--no-headers")
	if code != 0 || strings.Contains(output, "NAME") || !strings.Contains(output, resourceMachineID) {
		t.Fatalf("table: %d %q %s", code, output, diagnostic)
	}
	if paths[len(paths)-1] != "/v1/machines/"+resourceMachineID {
		t.Fatalf("UUID not used directly: %v", paths)
	}
}

func TestResourceApplyLocalDryRunAndValidation(t *testing.T) {
	requests := 0
	directory := resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		t.Error("client dry-run must make zero HTTP requests")
	})
	template := filepath.Join(directory, "template.yaml")
	if err := os.WriteFile(template, []byte("resources: []\nnumber: 9007199254740993\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, output, diagnostic := resourceRun("", "apply", "configuration", "docker", "--machine", "apphost01", "-f", template, "--dry-run=client", "-o", "json")
	if code != 0 || !strings.Contains(output, "9007199254740993") || strings.Contains(output, `"parameters"`) {
		t.Fatalf("plain dry-run: %d %s %s", code, output, diagnostic)
	}
	manifest := "apiVersion: dscreg/v1\nkind: Configuration\nmetadata:\n  name: docker\nspec:\n  machine: apphost01\n  template:\n    resources: []\n  parameters: {}\n"
	code, output, diagnostic = resourceRun(manifest, "apply", "-f", "-", "--dry-run=client", "-o", "yaml")
	documents, err := clidoc.Decode([]byte(output))
	if code != 0 || err != nil || len(documents) != 1 {
		t.Fatalf("manifest dry-run: %d %s %s %v", code, output, diagnostic, err)
	}
	for _, input := range []string{
		strings.Replace(manifest, "kind: Configuration", "kind: Machine", 1),
		strings.Replace(manifest, "  machine: apphost01", "  machine: apphost01\n  typo: true", 1),
		strings.Replace(manifest, "  parameters: {}", "  parameters: null", 1),
		manifest + "---\n" + manifest,
		strings.Replace(manifest, "resources: []", "resources: []\n    resources: []", 1),
	} {
		code, _, diagnostic = resourceRun(input, "apply", "-f", "-", "--dry-run=client")
		if code != 2 {
			t.Fatalf("invalid manifest accepted: %d %s", code, diagnostic)
		}
	}
	if requests != 0 {
		t.Fatalf("dry run issued %d requests", requests)
	}
}

func TestResourceManifestPrevalidatesAllBeforeWrites(t *testing.T) {
	writes := 0
	resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
		}
		fixtureMachine(w)
	})
	manifest := `{"apiVersion":"dscreg/v1","kind":"Configuration","metadata":{"name":"docker"},"spec":{"machine":"apphost01","template":{}}}`
	second := strings.Replace(manifest, "apphost01", resourceMachineID, 1)
	code, _, diagnostic := resourceRun(manifest+"\n---\n"+second, "apply", "-f", "-")
	if code != 2 || writes != 0 || !strings.Contains(diagnostic, "duplicate assignment") {
		t.Fatalf("aliased duplicate not rejected before mutation: %d writes=%d %s", code, writes, diagnostic)
	}
	invalid := strings.Replace(manifest, `"name":"docker"`, `"name":"../bad"`, 1)
	code, _, _ = resourceRun(manifest+"\n---\n"+invalid, "apply", "-f", "-")
	if code != 2 || writes != 0 {
		t.Fatal("invalid later manifest caused an earlier mutation")
	}
}

func TestResourceApplyReportsCreatedConfiguredUnchanged(t *testing.T) {
	for _, outcome := range []string{"created", "configured", "unchanged"} {
		t.Run(outcome, func(t *testing.T) {
			sequence := []string{}
			resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
				sequence = append(sequence, r.Method+" "+r.URL.Path)
				if r.URL.Path == "/v1/machines" {
					fixtureMachine(w)
					return
				}
				if r.Method == "GET" {
					if outcome == "created" {
						w.WriteHeader(404)
						resourceJSON(w, `{"error":{"code":"configuration_not_found","message":"missing"}}`)
					} else {
						resourceJSON(w, `{"name":"docker","revision":"old"}`)
					}
					return
				}
				var body map[string]any
				decoder := json.NewDecoder(r.Body)
				decoder.UseNumber()
				if err := decoder.Decode(&body); err != nil {
					t.Error(err)
				}
				if body["parameters"] == nil || body["template"].(map[string]any)["large"] != json.Number("9007199254740993") {
					t.Errorf("request lost precision or empty parameters: %#v", body)
				}
				if outcome == "created" {
					w.WriteHeader(201)
				}
				revision := "new"
				if outcome == "unchanged" {
					revision = "old"
				}
				resourceJSON(w, `{"name":"docker","revision":"`+revision+`"}`)
			})
			manifest := `{"apiVersion":"dscreg/v1","kind":"Configuration","metadata":{"name":"docker"},"spec":{"machine":"apphost01","template":{"large":9007199254740993},"parameters":{}}}`
			code, output, diagnostic := resourceRun(manifest, "apply", "-f", "-")
			if code != 0 || output != "configuration/docker "+outcome+"\n" || len(sequence) != 3 {
				t.Fatalf("%d %q %v %s", code, output, sequence, diagnostic)
			}
		})
	}
}

func TestResourceApplyStructuredMultipleIsOneDocument(t *testing.T) {
	resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/machines" {
			fixtureMachine(w)
		} else if r.Method == "GET" {
			w.WriteHeader(404)
			resourceJSON(w, `{"error":{"code":"configuration_not_found"}}`)
		} else {
			w.WriteHeader(201)
			resourceJSON(w, `{"name":"docker","revision":"new"}`)
		}
	})
	manifest := `{"apiVersion":"dscreg/v1","kind":"Configuration","metadata":{"name":"docker"},"spec":{"machine":"apphost01","template":{}}}`
	manifest += "\n---\n" + strings.Replace(manifest, "docker", "ssh", 1)
	code, output, diagnostic := resourceRun(manifest, "apply", "-f", "-", "-o", "json")
	var values []map[string]any
	if err := json.Unmarshal([]byte(output), &values); code != 0 || err != nil || len(values) != 2 {
		t.Fatalf("combined JSON: %d %s %s %v", code, output, diagnostic, err)
	}
}

func TestResourceReportsRouteAndRawBytes(t *testing.T) {
	content := "{ \"schemaVersion\": 1, \"large\": 9007199254740993 }\n"
	var paths []string
	resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v1/machines" {
			fixtureMachine(w)
			return
		}
		resourceJSON(w, content)
	})
	code, output, diagnostic := resourceRun("", "get", "report", resourceReportID, "--machine", "apphost01", "--content", "-o", "raw")
	if code != 0 || output != content || paths[1] != "/v1/machines/"+resourceMachineID+"/reports/"+resourceReportID+"/content" {
		t.Fatalf("raw: %d %q %v %s", code, output, paths, diagnostic)
	}
	paths = nil
	code, output, diagnostic = resourceRun("", "get", "report", "--machine", "apphost01", "--configuration", "docker", "--latest", "--content", "-o", "yaml")
	if code != 0 || !strings.Contains(output, "9007199254740993") || paths[1] != "/v1/machines/"+resourceMachineID+"/configurations/docker/report" {
		t.Fatalf("latest: %d %q %v %s", code, output, paths, diagnostic)
	}
	requests := len(paths)
	for _, args := range [][]string{
		{"get", "report", "--machine", "apphost01", "--content"},
		{"get", "report", "--machine", "apphost01", "--latest"},
		{"get", "report", resourceReportID, "--machine", "apphost01", "-o", "raw"},
	} {
		if code, _, _ := resourceRun("", args...); code != 2 {
			t.Fatalf("invalid flags accepted: %v", args)
		}
	}
	if len(paths) != requests {
		t.Fatal("invalid report flags made HTTP requests")
	}
}

func TestResourceDescribeMachineNoNPlusOneOrCompliance(t *testing.T) {
	requests := 0
	resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path == "/v1/machines" {
			fixtureMachine(w)
			return
		}
		resourceJSON(w, `{"configurationRevision":7,"outcome":"unknown","configurations":[{"name":"docker","desiredRevision":"a","revisionState":"current","outcome":"succeeded"},{"name":"ssh","desiredRevision":"b","revisionState":"stale","outcome":"unknown"},{"name":"monitoring","desiredRevision":"c","revisionState":"unreported","outcome":"never_reported"},{"name":"old","desiredRevision":null,"revisionState":"removed","outcome":"unknown"}]}`)
	})
	code, output, diagnostic := resourceRun("", "describe", "machine", "apphost01")
	for _, want := range []string{"Outcome:", "unknown", "Latest execution succeeded.", "Latest retained report is for a previous revision.", "No execution reported.", "Configuration is no longer desired."} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q: %s", want, output)
		}
	}
	if code != 0 || requests != 2 || strings.Contains(output, "compliant") || strings.Contains(output, "online") {
		t.Fatalf("%d requests=%d %s %s", code, requests, output, diagnostic)
	}
	if code, _, _ := resourceRun("", "describe", "machine", "apphost01", "-o", "json"); code != 2 || requests != 2 {
		t.Fatal("structured describe not rejected locally")
	}
}

func TestResourceDescribeConfigurationMissingReport(t *testing.T) {
	for _, state := range []string{"unreported", "stale", "removed", "current"} {
		t.Run(state, func(t *testing.T) {
			var paths []string
			resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				switch r.URL.Path {
				case "/v1/machines":
					fixtureMachine(w)
				case "/v1/machines/" + resourceMachineID + "/configurations/docker/status":
					w.WriteHeader(404)
					resourceJSON(w, `{"error":{"code":"report_not_found"}}`)
				case "/v1/machines/" + resourceMachineID + "/status":
					resourceJSON(w, `{"configurations":[{"name":"ssh","revisionState":"current"},{"name":"docker","revisionState":"`+state+`","latestReport":{"id":"`+resourceReportID+`","outcome":"succeeded"},"latestReceivedReport":{"configurationRevision":"old"}}]}`)
				default:
					resourceJSON(w, `{"name":"docker","revision":"current","template":{"sensitive":"do-not-display"},"parameters":{"password":"secret"}}`)
				}
			})
			code, output, diagnostic := resourceRun("", "describe", "configuration", "docker", "--machine", "apphost01")
			message := observation(state, "succeeded")
			if state == "unreported" {
				message = "No execution reported for the current revision."
			}
			if code != 0 || len(paths) != 4 || paths[3] != "/v1/machines/"+resourceMachineID+"/status" || !strings.Contains(output, message) {
				t.Fatalf("%d %v %s %s", code, paths, output, diagnostic)
			}
			if state == "stale" && (!strings.Contains(output, "stale") || strings.Contains(output, "unreported") || strings.Contains(output, "No execution reported")) {
				t.Fatalf("stale report mislabeled: %s", output)
			}
			if strings.Contains(output, "secret") || strings.Contains(output, "do-not-display") {
				t.Fatalf("describe leaked desired content: %s", output)
			}
		})
	}
}

func TestResourceDescribeConfigurationFallbackErrors(t *testing.T) {
	for _, failure := range []string{"configuration_not_found", "aggregate_unavailable", "missing_summary"} {
		t.Run(failure, func(t *testing.T) {
			requests := 0
			resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				switch r.URL.Path {
				case "/v1/machines":
					fixtureMachine(w)
				case "/v1/machines/" + resourceMachineID + "/configurations/docker/status":
					code := "report_not_found"
					if failure == "configuration_not_found" {
						code = failure
					}
					w.WriteHeader(404)
					resourceJSON(w, `{"error":{"code":"`+code+`"}}`)
				case "/v1/machines/" + resourceMachineID + "/status":
					if failure == "aggregate_unavailable" {
						w.WriteHeader(503)
						resourceJSON(w, `{"error":{"code":"database_unavailable"}}`)
					} else {
						resourceJSON(w, `{"configurations":[]}`)
					}
				default:
					resourceJSON(w, `{"name":"docker","revision":"current"}`)
				}
			})
			code, output, diagnostic := resourceRun("", "describe", "configuration", "docker", "--machine", "apphost01")
			if failure == "configuration_not_found" {
				if code != 3 || requests != 3 {
					t.Fatalf("unexpected fallback: %d %d %s", code, requests, diagnostic)
				}
			} else if code != 1 || requests != 4 {
				t.Fatalf("fallback error hidden: %d %d %s", code, requests, diagnostic)
			}
			if output != "" {
				t.Fatalf("failure produced success output: %q", output)
			}
		})
	}
}

func TestResourceExportRoundtripAndNoOverwrite(t *testing.T) {
	directory := resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/machines" {
			fixtureMachine(w)
		} else {
			resourceJSON(w, `{"name":"docker","revision":"omit-me","template":{"number":9007199254740993},"parameters":{}}`)
		}
	})
	code, output, diagnostic := resourceRun("", "export", "configuration", "docker", "--machine", "apphost01")
	if code != 0 || strings.Contains(output, "omit-me") {
		t.Fatalf("manifest: %d %s %s", code, output, diagnostic)
	}
	if code, _, diagnostic = resourceRun(output, "apply", "-f", "-", "--dry-run=client"); code != 0 {
		t.Fatalf("export not re-applicable: %s", diagnostic)
	}
	template := filepath.Join(directory, "docker.yaml")
	parameters := filepath.Join(directory, "docker.parameters.json")
	base := []string{"export", "configuration", "docker", "--machine", "apphost01", "--file", template}
	if code, _, _ = resourceRun("", base...); code != 2 {
		t.Fatal("parameters silently discarded")
	}
	if _, err := os.Stat(template); !os.IsNotExist(err) {
		t.Fatal("partial template written before validation")
	}
	args := append(base, "--parameters-file", parameters)
	if code, _, diagnostic = resourceRun("", args...); code != 0 {
		t.Fatalf("export files: %s", diagnostic)
	}
	got, err := os.ReadFile(template)
	if err != nil || !strings.Contains(string(got), "9007199254740993") {
		t.Fatal("precision lost in file export")
	}
	if code, _, _ = resourceRun("", args...); code != 2 {
		t.Fatal("existing files silently overwritten")
	}
	if code, _, diagnostic = resourceRun("", append(args, "--overwrite")...); code != 0 {
		t.Fatalf("explicit overwrite: %s", diagnostic)
	}
}

func TestResourceDeletionConfirmationAndIgnoreNotFound(t *testing.T) {
	deletions := 0
	resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deletions++
			w.WriteHeader(204)
			return
		}
		if r.URL.Query().Get("name") == "missing" {
			w.WriteHeader(404)
			resourceJSON(w, `{"error":{"code":"machine_not_found"}}`)
			return
		}
		fixtureMachine(w)
	})
	if code, _, _ := resourceRun("", "delete", "machine", "apphost01"); code != 2 || deletions != 0 {
		t.Fatal("noninteractive deletion bypassed confirmation")
	}
	code, output, diagnostic := resourceRun("", "delete", "machine", "apphost01", "--yes")
	if code != 0 || deletions != 1 || !strings.Contains(output, "deleted") {
		t.Fatalf("%d %s %s", code, output, diagnostic)
	}
	if code, output, _ := resourceRun("", "delete", "machine", "missing", "--ignore-not-found", "--yes"); code != 0 || output != "" {
		t.Fatal("ignore-not-found did not suppress missing resource")
	}
	code, _, diagnostic = resourceRun("", "delete", "api-key", "k_6AeG", "--machine", "apphost01", "--yes")
	if code != 0 || deletions != 2 {
		t.Fatalf("opaque key ID rejected: %d %s", code, diagnostic)
	}
}

func TestResourceCredentialsTTLAndSave(t *testing.T) {
	var request map[string]any
	requests := 0
	directory := resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path == "/v1/machines" {
			fixtureMachine(w)
			return
		}

		json.NewDecoder(r.Body).Decode(&request)
		w.WriteHeader(201)
		resourceJSON(w, `{"token":"dscr_secret","expiresAt":"2026-10-24T00:00:00Z","key":"dscm_secret","keyId":"k_test"}`)
	})
	token := filepath.Join(directory, "token")
	code, output, diagnostic := resourceRun("", "create", "registration-token", "--ttl", "30m", "--save-key-to", token, "-o", "json")
	if code != 0 || request["ttlSeconds"] != float64(1800) || strings.Contains(output+diagnostic, "dscr_secret") {
		t.Fatalf("%d %s %s %#v", code, output, diagnostic, request)
	}
	data, err := os.ReadFile(token)
	if err != nil || string(data) != "dscr_secret\n" {
		t.Fatal("token not saved")
	}
	count := requests
	for _, ttl := range []string{"0", "24h1s", "1500ms"} {
		if code, _, _ := resourceRun("", "create", "registration-token", "--ttl", ttl); code != 2 {
			t.Fatalf("invalid TTL %q accepted", ttl)
		}
	}
	if requests != count {
		t.Fatal("invalid TTL made HTTP request")
	}
	if code, _, _ := resourceRun("", "create", "api-key", "--machine", "apphost01", "--admin", resourceMachineID); code == 0 {
		t.Fatal("conflicting owners accepted")
	}
}

func TestResourceCredentialDestinationReservedBeforeRequests(t *testing.T) {
	for _, resource := range []string{"api-key", "registration-token"} {
		for _, destination := range []string{"existing", "missing-parent"} {
			t.Run(resource+"/"+destination, func(t *testing.T) {
				requests := 0
				directory := resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
					requests++
					t.Error("invalid credential destination made an HTTP request")
				})
				path := filepath.Join(directory, "credential")
				if destination == "existing" {
					if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					path = filepath.Join(directory, "missing", "credential")
				}
				args := []string{"create", resource, "--save-key-to", path}
				if resource == "api-key" {
					args = append(args, "--machine", "apphost01")
				}
				if code, _, diagnostic := resourceRun("", args...); code == 0 || requests != 0 {
					t.Fatalf("invalid destination: %d requests=%d %s", code, requests, diagnostic)
				}
				if destination == "existing" {
					data, err := os.ReadFile(path)
					if err != nil || string(data) != "original" {
						t.Fatal("existing credential destination changed")
					}
				}
			})
		}
	}
}

func TestResourceCredentialReservationAndCleanup(t *testing.T) {
	for _, resource := range []string{"api-key", "registration-token"} {
		for _, succeeds := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", resource, succeeds), func(t *testing.T) {
				var path string
				directory := resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1/machines" {
						fixtureMachine(w)
						return
					}
					info, err := os.Stat(path)
					if err != nil || info.Size() != 0 {
						t.Errorf("protected destination was not reserved before credential issuance: %v", err)
					}
					if !succeeds {
						w.WriteHeader(503)
						resourceJSON(w, `{"error":{"code":"database_unavailable"}}`)
						return
					}
					resourceJSON(w, `{"keyId":"k_test","key":"dscm_secret","token":"dscr_secret","expiresAt":"2026-10-24T00:00:00Z"}`)
				})
				path = filepath.Join(directory, "credential")
				args := []string{"create", resource, "--save-key-to", path}
				if resource == "api-key" {
					args = append(args, "--machine", "apphost01")
				}
				code, output, diagnostic := resourceRun("", args...)
				if strings.Contains(output+diagnostic, "dscm_secret") || strings.Contains(output+diagnostic, "dscr_secret") {
					t.Fatal("saved credential leaked in output")
				}
				if !succeeds {
					if code != 1 {
						t.Fatalf("request failure: %d %s", code, diagnostic)
					}
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("failed request left reserved destination: %v", err)
					}
					return
				}
				data, err := readPrivate(path)
				expected := "dscm_secret\n"
				if resource == "registration-token" {
					expected = "dscr_secret\n"
				}
				if code != 0 || err != nil || data != expected {
					t.Fatalf("saved credential: %d %v %s", code, err, diagnostic)
				}
			})
		}
	}
}

func TestResourceAPIKeyNameOutputRequiresSavedSecret(t *testing.T) {
	requests := 0
	directory := resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		resourceJSON(w, `{"keyId":"k_test","key":"dsca_secret"}`)
	})
	for _, owner := range []string{"--machine", "--admin"} {
		code, output, diagnostic := resourceRun("", "create", "api-key", owner, resourceMachineID, "-o", "name")
		if code != 2 || output != "" || requests != 0 || !strings.Contains(diagnostic, "--save-key-to") {
			t.Fatalf("one-time secret discarded: %d requests=%d %q %s", code, requests, output, diagnostic)
		}
	}
	path := filepath.Join(directory, "key")
	code, output, diagnostic := resourceRun("", "create", "api-key", "--admin", resourceMachineID, "-o", "name", "--save-key-to", path)
	key, err := readPrivate(path)
	if code != 0 || requests != 1 || output != "api-key/k_test\n" || err != nil || key != "dsca_secret\n" {
		t.Fatalf("saved name output: %d %q %v %s", code, output, err, diagnostic)
	}
}

func TestResourceApplyFailureContinuesAndPreflightFailureNeverWrites(t *testing.T) {
	for _, preflightFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(preflightFailure), func(t *testing.T) {
			writes := []string{}
			resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v1/machines":
					fixtureMachine(w)
				case r.Method == http.MethodGet:
					if preflightFailure && strings.HasSuffix(r.URL.Path, "/ssh") {
						w.WriteHeader(403)
						resourceJSON(w, `{"error":{"code":"forbidden"}}`)
					} else {
						resourceJSON(w, `{"name":"configuration","revision":"old"}`)
					}
				default:
					writes = append(writes, r.URL.Path)
					if strings.HasSuffix(r.URL.Path, "/docker") {
						w.WriteHeader(409)
						resourceJSON(w, `{"error":{"code":"conflict","message":"do-not-log-sensitive-server-data"}}`)
					} else {
						resourceJSON(w, `{"name":"ssh","revision":"new"}`)
					}
				}
			})
			manifest := `{"apiVersion":"dscreg/v1","kind":"Configuration","metadata":{"name":"docker"},"spec":{"machine":"apphost01","template":{}}}`
			manifest += "\n---\n" + strings.Replace(manifest, "docker", "ssh", 1)
			code, output, diagnostic := resourceRun(manifest, "apply", "-f", "-")
			if preflightFailure {
				if code != 5 || len(writes) != 0 {
					t.Fatalf("preflight failure: code=%d writes=%v %s", code, writes, diagnostic)
				}
			} else if code != 4 || len(writes) != 2 || !strings.Contains(output, "configuration/ssh configured") || !strings.Contains(diagnostic, "configuration/docker failed") {
				t.Fatalf("partial failure: code=%d writes=%v output=%s diagnostic=%s", code, writes, output, diagnostic)
			}
			if strings.Contains(output+diagnostic, "do-not-log-sensitive-server-data") {
				t.Fatal("server failure body leaked")
			}
		})
	}
}

func TestResourceStatusAndReportTableUseActualFields(t *testing.T) {
	resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/machines":
			fixtureMachine(w)
		case strings.HasSuffix(r.URL.Path, "/reports"):
			resourceJSON(w, `{"reports":[{"id":"`+resourceReportID+`","configurationName":"docker","configurationRevision":"revision","outcome":"failed","operation":"test","receivedAt":"2026-10-23T14:30:06Z"}]}`)
		default:
			resourceJSON(w, `{"reportId":"`+resourceReportID+`","configurationRevision":"revision","outcome":"succeeded","operation":"test","receivedAt":"2026-10-23T14:30:06Z","hasReport":true}`)
		}
	})
	for _, args := range [][]string{
		{"get", "status", "--machine", "apphost01", "--configuration", "docker"},
		{"get", "reports", "--machine", "apphost01"},
		{"get", "report", "--machine", "apphost01", "--configuration", "docker", "--latest"},
	} {
		code, output, diagnostic := resourceRun("", args...)
		for _, want := range []string{resourceReportID, "revision", "test", "2026-10-23T14:30:06Z"} {
			if code != 0 || !strings.Contains(output, want) {
				t.Fatalf("%v: missing %q: %d %s %s", args, want, code, output, diagnostic)
			}
		}
	}
}

func TestResourceAdministratorKeyRoutesAndOneTimeSecret(t *testing.T) {
	var methods []string
	resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/admins/"+resourceMachineID+"/api-keys" && r.URL.Path != "/v1/admins/"+resourceMachineID+"/api-keys/k_test" {
			t.Errorf("unexpected owner path: %s", r.URL.Path)
		}
		methods = append(methods, r.Method)
		switch r.Method {
		case http.MethodPost:
			resourceJSON(w, `{"keyId":"k_test","key":"dsca_new-secret","createdAt":"2026-10-23T00:00:00Z"}`)
		case http.MethodGet:
			resourceJSON(w, `{"keys":[{"keyId":"k_test","ownerType":"admin","ownerId":"`+resourceMachineID+`","revokedAt":null,"description":"rotation"}]}`)
		case http.MethodDelete:
			w.WriteHeader(204)
		}
	})
	code, output, diagnostic := resourceRun("", "create", "api-key", "--admin", resourceMachineID, "--quiet")
	if code != 0 || !strings.Contains(output, "dsca_new-secret") || !strings.Contains(diagnostic, "only once") {
		t.Fatalf("one-time secret hidden by quiet: %d %s %s", code, output, diagnostic)
	}
	code, output, diagnostic = resourceRun("", "get", "api-keys", "--admin", resourceMachineID, "-o", "yaml")
	if code != 0 || !strings.Contains(output, "rotation") || strings.Contains(output, "dsca_new-secret") {
		t.Fatalf("key listing: %d %s %s", code, output, diagnostic)
	}
	code, _, diagnostic = resourceRun("", "delete", "api-key", "k_test", "--admin", resourceMachineID, "--yes")
	if code != 0 || strings.Join(methods, ",") != "POST,GET,DELETE" {
		t.Fatalf("key routes: %d %v %s", code, methods, diagnostic)
	}
}

func TestResourceDescribeConfigurationCurrentExecution(t *testing.T) {
	resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/machines":
			fixtureMachine(w)
		case strings.HasSuffix(r.URL.Path, "/status"):
			resourceJSON(w, `{"reportId":"`+resourceReportID+`","configurationRevision":"revision","outcome":"failed","operation":"test","startedAt":"start","finishedAt":"finish","receivedAt":"received","errorKind":"dsc"}`)
		default:
			resourceJSON(w, `{"name":"docker","revision":"revision","template":{},"parameters":{"secret":"hidden"}}`)
		}
	})
	code, output, diagnostic := resourceRun("", "describe", "configuration", "docker", "--machine", "apphost01")
	for _, want := range []string{"current", "Latest execution failed.", "test", "start", "finish", "received", "dsc", resourceReportID, "dscreg get report"} {
		if code != 0 || !strings.Contains(output, want) {
			t.Fatalf("missing %q: %d %s %s", want, code, output, diagnostic)
		}
	}
	if strings.Contains(output, "hidden") || strings.Contains(output, "compliant") || strings.Contains(output, "offline") {
		t.Fatal("describe exposed sensitive data or inferred compliance/reachability")
	}
}

func TestResourceExportValidatesBothDestinationsBeforeWriting(t *testing.T) {
	directory := resourceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/machines" {
			fixtureMachine(w)
		} else {
			resourceJSON(w, `{"name":"docker","template":{},"parameters":{}}`)
		}
	})
	template := filepath.Join(directory, "template.json")
	parameters := filepath.Join(directory, "parameters.json")
	if err := os.WriteFile(parameters, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	code, _, diagnostic := resourceRun("", "export", "configuration", "docker", "--machine", "apphost01", "--file", template, "--parameters-file", parameters)
	if code != 2 {
		t.Fatalf("existing second destination accepted: %d %s", code, diagnostic)
	}
	if _, err := os.Stat(template); !os.IsNotExist(err) {
		t.Fatal("template written before validating parameter destination")
	}
}

func TestResourceHelpHasSummariesAndExamples(t *testing.T) {
	var out, diagnostic bytes.Buffer
	root := New(strings.NewReader(""), &out, &diagnostic)
	for _, verb := range []string{"get", "create", "delete", "apply", "export", "describe"} {
		command, _, err := root.Find([]string{verb})
		if err != nil || command.Short == "" || command.Example == "" {
			t.Fatalf("incomplete %s help: %v", verb, err)
		}
		for _, resource := range command.Commands() {
			if resource.Short == "" || resource.Example == "" {
				t.Errorf("incomplete help for %s %s", verb, resource.Name())
			}
		}
	}
	for _, args := range [][]string{{"apply", "--help"}, {"apply", "configuration", "--help"}} {
		code, output, diagnostic := resourceRun("", args...)
		if code != 0 || !strings.Contains(output, "Examples:") || !strings.Contains(output, "dscreg apply") {
			t.Fatalf("help %v: %d %s %s", args, code, output, diagnostic)
		}
	}
}
