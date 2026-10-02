# terraform-provider-vyoscmd

Manage raw VyOS `set` commands and `delete` assertions with Terraform or OpenTofu.
Use one `vyoscmd_commands` resource per router to commit a complete router change
in one API transaction. This avoids hundreds of separate resources and commits
for a large EVPN/VXLAN configuration.

Initial development version: **v0.1.0**. No release or registry package is published
by this project setup. Build locally with Go 1.25 or later. This implementation
uses Terraform Plugin Framework v1.19.0 and protocol 6; it uses no SSH or shell
execution on the router.

The canonical source address is `registry.terraform.io/houndsolo/vyoscmd`.

## Example

```hcl
terraform {
  required_providers {
    vyoscmd = {
      source  = "houndsolo/vyoscmd"
      version = "0.1.0"
    }
  }
}

variable "vyos_api_key" {
  type      = string
  sensitive = true
}

provider "vyoscmd" {
  endpoint = "https://10.20.11.11"
  api_key  = var.vyos_api_key
}

resource "vyoscmd_commands" "leaf_11" {
  name = "leaf-11"

  commands = [
    "set interfaces dummy dum240 address '10.255.240.11/32'",
    "set protocols bgp system-as 800",
  ]
}
```

OpenTofu resolves the short source above against `registry.opentofu.org`.
Use `source = "registry.terraform.io/houndsolo/vyoscmd"` to select the canonical
address in both tools. The local mirror instructions below support both forms.

For many leaves, set the endpoint on each resource:

```hcl
provider "vyoscmd" {
  api_key = var.vyos_api_key
}

resource "vyoscmd_commands" "leaf" {
  for_each = var.fabric.nodes.leaves
  name     = each.key
  endpoint = "https://${each.value.management_ip}"

  commands = [
    "set interfaces ethernet eth1 mtu 9189",
    "set interfaces ethernet eth2 mtu 9189",
    "set protocols bgp system-as 800",
    "delete protocols ospf",
  ]
}
```

This model permits resource `for_each` without a provider alias per router.
The provider supplies shared credentials, TLS policy and timeouts. Use aliases
when those settings differ. Changing a resource's resolved endpoint requires
replacement: the old router is cleaned up before the new router is configured.
Changes to hostname case, the explicit default port, or trailing URL separators
keep the same target and ID. Resource endpoint spelling can still cause an
in-place state update. Escaped slashes in proxy routes remain significant.
The endpoint saved in state remains the target for refresh and destroy even if
the provider default changes or is not yet known. An explicit unknown provider
endpoint does not use `VYOS_ENDPOINT` as a temporary fallback. A known resource
endpoint can still be used in this case. Do not use `create_before_destroy` for
overlapping configuration on the same router.

## Provider configuration

| Attribute | Default | Purpose |
|---|---|---|
| `endpoint` | `VYOS_ENDPOINT` | Default absolute router URL. Optional when resources supply endpoints. |
| `api_key` | `VYOS_API_KEY` | Sensitive API key. Required during apply. |
| `insecure` | `false` | Disable TLS certificate verification for lab certificates. |
| `request_timeout` | `"120s"` | Timeout for each API request during refresh and apply. |
| `plan_timeout` | `"5s"` | Total timeout for each resource's operation preview, including lock wait. |

Explicit settings take precedence over environment variables. Prefer a trusted
certificate and HTTPS. HTTP is accepted for local test servers or a trusted
proxy; it sends the API key without transport encryption. URLs cannot contain
user credentials, query parameters, or fragments. Redirects are rejected.
If no endpoint is configured and no endpoint value is pending, plan reports a
configuration error. An unreachable or not-yet-known endpoint still permits a
best-effort plan.

Enable the VyOS HTTPS REST API before apply. The key needs configuration read,
configure, and save access. See the [VyOS API documentation](https://docs.vyos.io/en/rolling/automation/vyos-api.html).

## Resource: vyoscmd_commands

| Attribute | Type | Behavior |
|---|---|---|
| `name` | string, required | Human-readable log label; renaming it keeps the ID. |
| `endpoint` | string, optional/computed | Resource URL or provider default. Stored in state. |
| `commands` | set(string), required | Desired `set` commands and explicit `delete` assertions. Use `[]` to remove managed SET configuration. |
| `save` | bool, optional | Default `true`. Save after successful create, update, or destroy. |
| `id` | string, computed | Random opaque ID, stable across updates and drift. |
| `managed_commands` | set(string), computed | Ownership ledger; normally the desired SET commands. |
| `in_sync` | bool, computed | False after drift or an incomplete save. Apply plans this to true. |
| `pending_save` | bool, computed | Remembers an unsuccessful or uncertain save across refresh. |

The tokenizer supports whitespace, single quotes, double quotes, escaped
characters and joined quoted segments. Single quotes preserve backslashes.
Within double quotes, backslashes escape `"`, `\`, `$`, and backticks; other
backslashes remain literal. Outside quotes, a backslash escapes the next
character. There is no variable, command, or shell expansion. Empty quoted final
values are accepted; VyOS must allow them for the selected node.

Reordering commands has no effect. Equivalent quoting produces no router
operations, although Terraform can still update the literal command strings in
state. Internally, paths are compared as token arrays, not text prefixes.

Only `set` and `delete` are accepted. `delete protocols ospf` and a SET below
`protocols ospf` in the same desired set produce a validation error. There is no
import operation: create with the exact desired commands to adopt configuration
that already satisfies them, without replaying those SETs.

## Apply and drift

1. Read a complete configuration snapshot.
2. Compare state ownership, desired assertions, and the snapshot.
3. Delete removed owned values, prune safe parents, and add missing SETs.
4. Submit the entire sorted list, with DELETEs first, in **one `/configure`
   request**. A satisfied resource sends no configure request.
5. Read again after a configure request and verify convergence.
6. If `save = true`, call `/config-file` with `op = save`.

Saving persists the configuration; it is a separate API request and does not add
another configuration commit. Even a satisfied apply saves when `save = true`.
This provider never splits a batch or retries a configuration POST automatically.

Refresh keeps `commands` and the ownership ledger. It sets `in_sync = false`
when a SET is missing, a DELETE assertion is violated, a previous removal is
incomplete, or a save is pending. A normal plan shows `in_sync: false -> true`
and schedules an update. The debug preview shows the actual operations.
`-refresh=false` skips this normal drift check.

An explicit DELETE is an **absence assertion** and authorizes removal of its
stated subtree, including configuration created outside Terraform. It is not
expanded to broader parents. Removing the assertion stops enforcing absence.
Destroy never recreates deleted configuration or enforces DELETE assertions.

After a failed or uncertain mutation, state retains the union of old and
attempted SET ownership. Apply always reads again, so it can reconcile an
already-committed change instead of blindly replaying it. A failed save remains
pending across refresh. Terraform may taint a resource after a failed create;
standard Terraform replacement behavior then applies. For an uncertain commit,
wait for the router's operation to finish before retrying.

## Delete resolution and ownership safety

The provider requests `/retrieve` with `op = showConfig`, `path = []`, and
`configFormat = json_ast`. The AST retains node/value boundaries, empty nodes,
and comments. The decoder rejects missing or unknown structural fields instead
of treating incomplete data as an empty router. There is no schema-name table
for addresses, MTU, BGP, or policy nodes.

Each snapshot also reads `/show` with `show configuration commands` and requires
its SETs and comments to match the AST. The operational command reads the active
configuration; the AST provides structural information. A pending API session,
an incomplete export, or disagreement between the two reads causes an error.
Quoted multiline values are parsed as complete commands. Native VyOS exports
can use configuration-string escapes inside single quotes, and can leave
embedded apostrophes unescaped. These are not equivalent to shell quoting.
The provider rejects single-quoted backslashes and joined quoted segments in
active exports because they can otherwise make different active and pending
values appear equal. This can block a read or apply even when the affected value
is unmanaged. Full support for these exports needs an unambiguous active
configuration representation and tests against real VyOS versions. The user
command tokenizer still supports its documented quoting rules.

An unsupported export verb, such as a deactivation marker, also causes an error
rather than being silently ignored.

The snapshot contains every terminal SET path. A removed state command is
eligible for deletion only if that **exact terminal assertion** still exists.
If all active values of a node are being removed, deleting the node is safe.
Otherwise, delete only the selected value.

For an MTU replacement, this produces:

```text
DELETE interfaces dummy dum99 mtu
SET interfaces dummy dum99 mtu 1450
```

If another address must remain, removal produces:

```text
DELETE interfaces dummy dum99 address 192.0.2.1/32
```

A singleton address can safely use a node delete when no other value is present.
This avoids trying to infer scalar versus multi-value schema from a single
value. Current VyOS `exists` can return true for both scalar and multi-value
paths with values; it is not a reliable schema discriminator. See the upstream
[exists implementation](https://github.com/vyos/vyos-1x/blob/rolling/python/vyos/config.py)
and [CLI deletion implementation](https://github.com/vyos/vyatta-cfg/blob/current/src/cstore/cstore.cpp).

Parent pruning compares removable-terminal counts with total active-terminal
counts for each path prefix. A parent can replace its descendant DELETEs only
when every terminal below it is owned and being removed. Pruning stops at an
unmanaged value, a retained desired subtree, or an unmanaged comment. It never
reaches the configuration root. Redundant descendant DELETEs are then removed.

This removes a whole policy rule when its action and regex are removed together.
It can delete `policy` when all terminals below `policy` are being removed.
If an unmanaged rule match remains, only the owned action is removed. VyOS may
reject that incomplete rule; the provider preserves the unmanaged match and
returns an error. Expand ownership or adjust the rule explicitly to resolve it.

Further ownership rules:

- An existing SET that matches desired configuration is adopted on create.
- A parent SET asserts that the parent exists. It does not own its descendants.
- A state value changed outside Terraform is not deleted on destroy or when
  removed from configuration. A retained desired scalar SET can overwrite drift
  when VyOS applies that SET.
- Unmanaged comments block parent deletion. Deleting the last value of a
  commented node fails conservatively. An explicit DELETE can authorize removal.
- Do not overlap SET ownership between resources or independent state files.
- Per-router locks cover read/commit/verify/save within one provider process,
  including aliases. They do not lock a human CLI session, another process,
  or two different URLs for the same router. Use one writer during apply.

## Debug operation preview

```bash
TF_LOG_PROVIDER=DEBUG tofu plan
```

The provider logs a batch header with `phase`, `change`, `resource`, and
`operation_count`. Each operation has an index, verb, and command. Delete
resolution logs include `source_set_path`, `delete_path`, and `reason`.

Preview is best effort. An unknown VM endpoint, unavailable API, timeout, or
unusable snapshot produces an unavailable-preview debug message, not a plan
error. Existing-resource refresh still fails if it cannot read the router;
the provider does not silently claim drift was checked. Apply recomputes the
batch even when applying a saved plan.

API keys and HTTP bodies are not logged. Common credential paths are redacted
in operation previews. API error bodies are omitted because they can echo
credentials; diagnostics direct you to the router logs. Arbitrary raw commands
can contain secrets that no generic provider can identify. Treat Terraform
state and debug output as sensitive. The commands and ownership ledger are
stored in state. Do not put credentials in resource names or URLs.

## Performance and API limits

There are no per-command existence probes. Each snapshot uses two requests:
the structural AST and the active command export. Refresh and exact preview
each use one snapshot. An apply with changes normally uses two snapshots,
one configure request, and optionally one save request: at most six requests.
A satisfied apply needs one snapshot and optionally one save, at most three
requests. Snapshot indexing and parent
counts are computed locally; `BenchmarkLargeBatch` covers 2,000 changed commands.

The AST format is required in v0.1.0; there is no fallback to an ambiguous or
partial representation. It is exposed by the current VyOS REST
[retrieve endpoint](https://github.com/vyos/vyos-1x/blob/rolling/src/services/api/rest/routers.py).
The REST session must be idle when it is read. Matching both representations
guards against a pending session but is not a server-side transaction lock.
Concurrent writers can still change the router after a snapshot. The API does
not provide an optimistic concurrency token for this provider's commit.

Large atomic commits can exceed a router or reverse-proxy timeout or request
body limit. Raise those limits and `request_timeout` for a large fabric. The
provider keeps the one-resource/one-configure guarantee instead of silently
splitting the update. See the [VyOS bulk API guidance](https://docs.vyos.io/en/rolling/automation/vyos-api.html#bulk-configuration).

## Build and test

```bash
make fmt
make vet
make test
make build
make race
go test ./internal/provider -run '^$' -bench BenchmarkLargeBatch -benchmem
```

`make build` writes `bin/terraform-provider-vyoscmd_v0.1.0` and embeds version
`0.1.0` with Go linker flags. Binaries, state, variable files, and local CLI
configuration are ignored by Git.

Unit tests cover parsing, quoting, scalar replacement, address removal,
absence assertions, contradictions, pruning, comments, drift, ownership,
atomic HTTP batches, TLS, redirects, failure recovery, cancellation, logging,
and the Framework protocol lifecycle. They use local fake APIs, not a router.

The CLI test also uses a fake API:

```bash
make build
python3 scripts/test_cli.py --cli tofu
python3 scripts/test_cli.py --cli terraform
```

It checks normal plans, a VM output unknown during plan, drift, apply of a saved
plan after a router change, pruning, and destroy. GitHub Actions runs Go tests,
the race detector, vet, format checks, and both CLI tests. No release automation
is configured.

## OpenTofu filesystem mirror

```bash
make install-local
```

This installs the binary under both default registry names at:

```text
~/.local/share/terraform/providers/registry.terraform.io/houndsolo/vyoscmd/0.1.0/linux_amd64/terraform-provider-vyoscmd_v0.1.0
~/.local/share/terraform/providers/registry.opentofu.org/houndsolo/vyoscmd/0.1.0/linux_amd64/terraform-provider-vyoscmd_v0.1.0
```

The Makefile selects your actual OS and architecture. Set `MIRROR` to use a
different install directory. In a local CLI configuration file, use an absolute
path; replace `/home/ryan` below with your home directory:

```hcl
provider_installation {
  filesystem_mirror {
    path = "/home/ryan/.local/share/terraform/providers"
    include = [
      "registry.terraform.io/houndsolo/vyoscmd",
      "registry.opentofu.org/houndsolo/vyoscmd",
    ]
  }
  direct {
    exclude = [
      "registry.terraform.io/houndsolo/vyoscmd",
      "registry.opentofu.org/houndsolo/vyoscmd",
    ]
  }
}
```

```bash
export TF_CLI_CONFIG_FILE="$HOME/.config/opentofu/vyoscmd.tfrc"
tofu init
TF_LOG_PROVIDER=DEBUG tofu plan
```

Keep that CLI configuration outside the repository. A rebuilt binary has a new
checksum: use a new development version or refresh this provider's local lock
entry and installation in a disposable test root. Do not remove a production
lock file or state just to bypass a checksum mismatch.

## Real-router validation and v0.2.0

No test in this repository requires or proves behavior against a real VyOS VM.
Before managing a fabric, test the exact VyOS image with a disposable dummy
interface and policy rule. Verify the AST response, singleton and multiple
addresses, empty flag nodes, default-valued nodes, comments, quoted values,
policy deletion, save failures, and rollback after a rejected batch.

Schema defaults and VyOS value normalization can prevent literal convergence.
For example, deleting a default-valued node can restore its default rather than
make the node absent. Two different SET values for the same scalar node cannot
be identified reliably without schema information. The provider checks the
result and returns an error instead of claiming convergence. HTTPS-service
changes can commit asynchronously or break the provider connection. They need
separate real-router testing. An HTTP success alone is not treated as proof
that the desired configuration was reached.

Useful v0.2.0 work: an automated VyOS image test matrix, schema-aware validation
and normalization, explicit policies for default nodes, commit-confirm support,
bounded polling for asynchronous commits, and an API-supported optimistic
concurrency check. Import could accept an explicit ownership manifest; it must
not adopt a whole router implicitly.
