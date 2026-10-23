# DSC Registry administration with dscreg

`dscreg` is a standalone, cross-platform HTTP administration client for DSC
Registry. It does not need a local Registry, DSC executable, database, or report
directory. `dscregd` serves the Registry; the separate machine synchronization
agent (`dscregsyncd`, called `dscsyncd` in the server architecture) registers
machines and synchronizes files/results; `dscd` performs local reconciliation.
The administration CLI does not execute configurations or manage agent-local
settings.

## Installation

Release archives include `dscreg` alongside `dscregd`. Put the executable on
`PATH`. Linux DEB/RPM packages install it on `PATH`, and the Windows installer
includes `dscreg.exe`. The CLI is never installed as a service. Alternatively:

```sh
go build -o dscreg ./cmd/dscreg
dscreg version
```

`version` and `--version` print the build version and source commit without
connecting to a server.

## Connection setup

```sh
dscreg config set-context homelab --server http://apphost01:8080
dscreg config use-context homelab
dscreg ping
```

Readiness does not validate administrator authentication. HTTP is supported but
warns because credentials are transmitted unencrypted. Prefer HTTPS; system
certificate trust is used by default. `--tls-ca-file ca.pem` adds a trusted CA.
`--insecure-skip-tls-verify` explicitly disables certificate validation and warns.
`--timeout 30s` bounds each HTTP request; interrupting the CLI cancels requests.

## Authentication

Bootstrap the first administrator locally with `dscregd bootstrap`, not with
`dscreg`. Feed its administrator API key into stdin without putting it in process
arguments or shell history:

```sh
dscreg auth login --api-key-stdin
```

Supply the key on stdin and end input (or pipe it from a protected secret store).
Saved credentials are separate from contexts. New credential files use mode
`0600` on POSIX and a current-user-only protected ACL on Windows. Existing key
files must be private; symbolic links and files accessible to other users are
rejected. Configuration output never includes saved plaintext credentials.

For automation, use `DSCREG_API_KEY`, `DSCREG_API_KEY_FILE`, or
`--api-key-file /protected/admin.key`. There is deliberately no raw `--api-key`
argument. `dscreg auth logout` removes the selected saved credential; it does not
revoke the server-side key or clear environment credentials.

Connection precedence is explicit flags, environment, selected context, then
built-in defaults. There is no default server. Flags include `--server`,
`--context`, `--config`, and `--api-key-file`; environment equivalents are
`DSCREG_SERVER`, `DSCREG_CONTEXT`, `DSCREG_CONFIG`, and `DSCREG_API_KEY_FILE`.
An explicit key-file flag takes precedence over `DSCREG_API_KEY`; at environment
priority the inline key takes precedence over the environment key-file setting.

## Machine management

```sh
dscreg get machines
dscreg get machine apphost01 -o json
dscreg create machine testmachine
dscreg delete machine testmachine --yes
```

Machine identifiers accept canonical UUIDs or exact case-insensitive names.
Names are resolved through the server's indexed lookup, not a fleet scan;
subsequent requests use immutable UUIDs. Deletion removes associated desired
configurations, reports, and credentials. Destructive commands require terminal
confirmation unless `--yes` is given. Use `--ignore-not-found` for idempotent
deletion. Removing desired state does not undo changes already applied locally.

## Applying configuration documents

```sh
dscreg apply configuration docker --machine apphost01 \
  -f docker.yaml --parameters-file docker.parameters.yaml
dscreg apply configuration packages --machine apphost01 -f packages.json
dscreg apply configuration ssh --machine apphost01 -f ssh.yaml --dry-run=client
dscreg get configurations --machine apphost01
dscreg get configuration docker --machine apphost01 -o yaml
dscreg delete configuration ssh --machine apphost01
```

Plain files are ordinary DSC object documents, not Registry manifests. JSON and
YAML are translated into JSON objects for the existing HTTP API. Parameter files
must also be objects. Omitting parameters removes the sidecar; an explicit `{}`
is a distinct empty sidecar. Numeric precision and opaque nested DSC properties
are preserved. Duplicate keys, unsupported YAML structures, invalid shapes, and
non-string mapping keys are rejected. Source files are never rewritten.

Apply prints `created`, `configured`, or `unchanged` using returned content
revisions rather than assuming HTTP 200 means a change. The server owns
canonicalization and no-op detection. Client dry-run only parses and validates
local structure: it makes no server mutation or DSC semantic validation.

## Declarative manifests

```yaml
apiVersion: dscreg/v1
kind: Configuration
metadata:
  name: docker
spec:
  machine: apphost01
  template:
    $schema: https://aka.ms/dsc/schemas/v3/bundled/config/document.json
    resources: []
  parameters:
    ContainerRuntime: docker
```

```sh
dscreg apply -f registry.yaml
dscreg apply -f docker-registry.yaml -f ssh-registry.yaml
dscreg apply -f -
dscreg apply -f registry.yaml --dry-run=client
```

Multi-document YAML (`---`) is supported. Registry wrapper fields are strict;
embedded templates remain opaque. Duplicate machine/configuration assignments
in one invocation are errors. There is no pruning: names omitted from manifests
are not deleted. Applications are individual API mutations, not a cross-machine
transaction; a failure produces a nonzero exit code.

## Export and reapply

```sh
dscreg export configuration docker --machine apphost01 > docker-registry.yaml
dscreg export configuration docker --machine apphost01 -o json > docker-registry.json
dscreg apply -f docker-registry.yaml
dscreg export configuration docker --machine apphost01 \
  --file docker.dsc.yaml --parameters-file docker.parameters.yaml
```

Manifest export includes the machine name and desired documents, without
server-generated revision/timestamp metadata. Raw export writes template and
parameters separately; a parameter destination is required when a sidecar
exists. File extensions select JSON/YAML unless output format is explicit.
Existing files require `--overwrite`. Exported documents and reports can contain
secrets; protect their destination and any redirected output.

## Describe and execution observations

```sh
dscreg describe machine apphost01
dscreg describe configuration docker --machine apphost01
```

Describe is human-readable only; use `get -o json` or `get -o yaml` for automation.
Machine describe combines identity with one aggregate status request, not one
request per configuration. Configuration describe shows desired revision,
execution summary, report reference, and observations, without templates,
parameters, or full report content.

`current`, `stale`, `unreported`, and `removed` distinguish desired revisions
from retained receipts. A missing current-revision report is normal and is
explained as such. The Registry retains only the latest accepted report per
configuration name; a delayed older-revision report can replace a current one
and make status stale. Receipt order is not execution chronology.

Execution `succeeded` is not proof of compliance: a successful DSC `test` may
report drift. Neither report timestamps nor machine existence establish
reachability, online status, or health.

## Status and reports

```sh
dscreg get status --machine apphost01
dscreg get status --machine apphost01 --configuration docker -o json
dscreg get reports --machine apphost01 --limit 100
dscreg get report REPORT_UUID --machine apphost01
dscreg get report REPORT_UUID --machine apphost01 --content -o raw
dscreg get report --machine apphost01 --configuration docker --latest --content -o yaml
```

Report lists are latest retained receipts, not history. Individual metadata is
separate from sensitive full content. Latest-by-configuration content selects
the current desired revision; stale receipts remain inspectable by their
retained report ID. `-o raw` preserves original report bytes; JSON/YAML
serialization preserves JSON values and additive fields, not whitespace.

## Registration tokens

```sh
dscreg create registration-token
dscreg create registration-token --ttl 30m
dscreg create registration-token --ttl 1h --save-key-to ./registration-token
```

Tokens default to one hour; TTL must be a whole number of seconds from 1 through
86400. Plaintext is returned only once. `--save-key-to` creates a new protected
file instead of printing the secret in human-readable output. There are no
token-list/revoke commands or machine self-registration in this CLI.

## API-key rotation

```sh
dscreg create api-key --machine apphost01 --description "replacement sync credential" \
  --save-key-to ./replacement.key
dscreg get api-keys --machine apphost01
# Install and verify the replacement credential before revoking the old one.
dscreg delete api-key OLD_KEY_ID --machine apphost01
dscreg create api-key --admin ADMIN_UUID --description "administrator rotation"
dscreg get api-keys --admin ADMIN_UUID
dscreg delete api-key OLD_KEY_ID --admin ADMIN_UUID
```

`--machine` and `--admin` are mutually exclusive. Newly issued keys are shown
once or saved to a protected file. Listings contain metadata only. Revoking the
last administrator key requires recovery through local `dscregd bootstrap`.

## Output and scripting

```sh
dscreg get machines -o wide
dscreg get machines --no-headers
dscreg get machines -o name
dscreg get machines --limit 100 --cursor MACHINE_UUID -o json
dscreg get machines --all -o json
dscreg get configurations --machine apphost01 -o yaml
```

Supported formats are `table` (default), `wide`, `json`, `yaml`, and `name`;
`raw` is only for report content. Structured output is one parseable document,
retains API field names/types and pagination `nextCursor`, and never mixes in
friendly messages. `--all` explicitly combines all pages and removes the cursor
after reaching the last page. Default page size is 100, maximum 1000. Lists do
not silently enumerate the entire fleet or enrich each entry with extra calls.
Warnings, errors, and redacted `--verbose` diagnostics go to stderr.
`--quiet` suppresses routine human success messages, not requested data.

Exit codes: `0` success; `1` request/server/transport failure; `2` invalid command
or input; `3` not found; `4` conflict; `5` authentication/authorization failure.

Resource aliases include `machine`/`machines`/`mc`,
`configuration`/`configurations`/`cfg`, `status`/`statuses`,
`report`/`reports`, `api-key`/`api-keys`, and
`registration-token`/`registration-tokens`. `config` always means local contexts.

## Context management

```sh
dscreg config set-context staging --server https://staging.example.com
dscreg config use-context homelab
dscreg config current-context
dscreg config get-contexts
dscreg config view
dscreg config delete-context staging
dscreg --context staging get machines
```

The default file is `$XDG_CONFIG_HOME/dscreg/config.yaml`, falling back to
`~/.config/dscreg/config.yaml` on Linux/macOS, and
`%APPDATA%\dscreg\config.yaml` on Windows. `--config` overrides `DSCREG_CONFIG`.
Contexts contain server URLs and credential references, not server database
settings. Log out before changing a saved context's server to avoid sending
credentials to a different destination. Login refuses server overrides.

## Shell completion and help

```sh
dscreg completion bash > dscreg.bash
dscreg completion zsh > _dscreg
dscreg completion fish > dscreg.fish
dscreg completion powershell > dscreg.ps1
dscreg help
dscreg help apply
dscreg help configuration
```

Source/install the generated script using your shell's completion conventions.
Completion is local; it does not enumerate remote resources.
