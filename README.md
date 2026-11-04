# configsctl

configsctl runs OpenTofu with declared infrastructure locks and reads or updates encrypted Vault secrets.

## Run it from a configs checkout

Run configsctl from the root of a configuration checkout. The selected workspace supplies its OpenTofu variables, and configsctl reads matching secret values from the encrypted Vault.

```sh
go run goodkind.io/configsctl/cmd/configsctl@latest tofu <workspace> plan
```

Run `configsctl lint` to check native configuration formatting and evaluate each workspace's nonsecret input metadata. Explicit file arguments retain the generic template linter. `version` prints the build commit, checkout state, and binary hash.

## Select an OpenTofu workspace

A workspace is a directory with a `backend` block inside a `terraform` block in one of its `*.tf` or `*.tf.json` files. Its selector is the complete relative path below `tofu.workspaces_dir`, such as `suburban/tack-qa`. Workspace roots can have any nesting depth. A directory without a backend is not a workspace.

configsctl checks the selected root without parsing unrelated roots. Workspace discovery skips hidden directories, provider caches, and reusable `modules` directories. Selectors must use clean relative paths without parent traversal or directory symlinks.

`configsctl.yml` at the root of the configs checkout has two required keys:

```yaml
tofu:
  workspaces_dir: opentofu
  env_key_prefix: tofu_env_
```

- `workspaces_dir` is the path of the directory with the workspaces, relative to the root of the checkout.
- `env_key_prefix` selects the vault keys that configsctl exports as plain environment variables. A vault key `<env_key_prefix><NAME>` exports as `<NAME>`.

The optional `tofu.provider_mirror` setting selects a shared filesystem mirror.
An absolute path selects that directory directly. A relative path resolves
from the settings file directory. A missing directory refuses the command.
configsctl discovers provider source addresses from directory metadata and
writes a credential-free CLI configuration in the standard user cache
directory. OpenTofu installs those sources from the mirror and uses the public
registries for other sources. The generated configuration uses private
directories and files and takes precedence over `TF_CLI_CONFIG_FILE` when this
setting is present. For every `configsctl tofu` command except an `apply` on
the explicit lock path, configsctl passes the process environment and the
vault values to OpenTofu with these changes:

- configsctl expands `TF_CLI_ARGS` and every `TF_CLI_ARGS_*` variable into
  OpenTofu arguments and removes those variables from the environment.
- When a command uses a `tofu.credentials` entry, configsctl removes the
  `TF_VAR_` entries for these items:
  - The username variable
  - The password variable
  - The rotator password key

For commands that use a `tofu.credentials` entry, configsctl then adds the
generated username and password. When the configuration does not set
`tofu.provider_mirror`, these commands keep `TF_CLI_CONFIG_FILE` and the other
native CLI configuration variables from the process. The explicit lock path
passes only the variables listed in the Declare lock endpoints section.

To run OpenTofu in a nested workspace, pass its selector as the first argument:

```sh
configsctl tofu suburban/tack-qa plan
```

To run OpenTofu in `workspaces_dir` itself, omit the workspace name:

```sh
configsctl tofu plan
```

configsctl passes the remaining arguments to OpenTofu. Existing immediate-child selectors still work. A command without a workspace selector uses the backend in `workspaces_dir`; if that backend is absent, the command fails and lists the available selectors. Nested roots with the same final directory name require their complete selectors.

configsctl exports a vault key as `TF_VAR_<key>` only when the selected workspace declares a variable with that name.

## Require merged OpenTofu source

Commands that write deployment state or rotate provider credentials require a clean configuration checkout at a commit contained in `origin/main`. configsctl fetches `origin` and rejects unmerged commits, uncommitted files, and unavailable remotes before acquiring deployment locks or starting OpenTofu. Select the workspace through configsctl rather than the OpenTofu `-chdir` option. Preview plans that do not rotate credentials and other read-only commands remain available from unmerged checkouts.

When a plan writes an `-out` file, configsctl records its source commit, workspace, and committed or preview status in the local user cache, using the plan's SHA-256 digest as the key. A saved-plan apply requires those exact bytes and the same clean, merged configuration commit. Plans from dirty checkouts remain previews. Source inspection failures or configuration changes during planning refuse the saved plan. Regenerate plans that lack a source record, including older plans and plans copied to a machine without the matching cache entry.

## Scope infrastructure locks

An ordinary native `apply` or `destroy` prepares a saved plan before selecting locks. configsctl preserves the confirmation prompt unless the effective arguments include `-auto-approve`. It applies that exact plan after acquiring its declared locks. Unknown ownership refuses the mutation.

Guest-local changes use the enrolled guest's existing SSH lock. A known node and numeric VMID without deployment enrollment use a guest-specific lock directory through the declared hypervisor's SSH connection. Explicit resource ownership selects the host lock for guest creation, shared host changes, and firmware. `lock_free_types` can define a root that does not need Proxmox locks.

configsctl expands `TF_CLI_ARGS` and command-specific arguments once, using OpenTofu's argument order. Explicit arguments take precedence over environment arguments.

## Declare lock endpoints

Set `tofu.configuration_root` to the provider-free OpenTofu root that accepts a constant `workspace` variable and exports `lock_targets`. The selector matches the selected workspace below `tofu.workspaces_dir`.

```yaml
tofu:
  workspaces_dir: opentofu
  configuration_root: opentofu/render/configuration
  env_key_prefix: vault_tofu_env_
```

The native root selects only the requested feature module. Initialization, planning, and saved-plan reading receive the same selector. Evaluation uses a private module data directory and does not require state, providers, Vault values, or host access. Diagnostics use standard error and preserve OpenTofu JSON output on standard output. Missing native inputs refuse the command.

`configuration_root` and `lock_targets` cannot both appear in the settings. Native output uses the same ownership and endpoint fields as the static settings below. Native configuration workspaces preserve ordinary apply, destroy, variable arguments, refresh-only planning, diagnostic options, and recorded saved-plan applies. A scoped command accepts `-lock=true` and refuses `-lock=false`. The existing static remote-endpoint path retains its explicit argument restrictions.

The `tofu.lock_targets.<workspace>.lock_endpoints` setting maps each lock host name to an object with connection fields. The workspace can declare these keys beside `lock_endpoints`:

- `module_hosts`
- `module_key_hosts`
- `node_hosts`
- `guest_hosts`
- `lock_free_types`

```yaml
tofu:
  lock_targets:
    <workspace>:
      node_hosts:
        site: hv_alpha
      guest_hosts:
        site/101: guest-alpha
      lock_endpoints:
        hv_alpha:
          address: hv-alpha.example
        guest-alpha:
          user: deploy
          address: guest-alpha.example
```

Each endpoint provides the SSH connection for a lock host.

Endpoint fields:

| Field | Rule |
| --- | --- |
| `user` | configsctl uses this optional field as the SSH user. configsctl defaults to `root`. |
| `address` | A remote endpoint requires a nonempty SSH address. |
| `local` | Set this field to `true` to run the existing lock script on the controller. A local endpoint cannot also set an address. |

configsctl uses `ssh` as `user@address` for a remote endpoint. An explicitly local endpoint runs the same lock script on the controller. configsctl uses these lock directories:

| Lock | Host | Directory |
|---|---|---|
| Host lock | The lock host | `/var/lib/configs` |
| Guest lock | The declared hypervisor | `/var/lib/configs/guests/<vmid>` |

The plan decision selects a guest lock for a node and VMID without deployment enrollment. The guest lock uses the host name `guest:<node>/<vmid>`.

configsctl checks these rules when it reads the settings and refuses invalid settings before it starts OpenTofu:

- A declared `lock_endpoints` map must contain at least one endpoint.
- Each lock host name must contain at least one character.
- Each lock host name must match a host value in `module_hosts`, `module_key_hosts`, `node_hosts`, or `guest_hosts` in the same workspace.
- When the workspace declares any endpoint, every host value in those four mappings must have an endpoint. configsctl checks host names in sorted order and reports the first host without an endpoint, together with the first mapping entry that uses that host.

Static settings with remote `lock_endpoints` use the existing explicit lock path for supported `apply` commands. Except for the help requests below, configsctl refuses an `apply` or `destroy` command instead of using the inventory lock path when any condition in the table applies.

Refusal reasons:

| Condition | Reason |
| --- | --- |
| The command is `destroy`. | The explicit lock path does not support `destroy`. |
| The workspace does not set `tofu.guest_modules`. | The explicit lock path requires `tofu.guest_modules`. |
| The command line does not start with `apply`. | The explicit lock path requires a command line that starts with `apply`. |
| The command line includes a saved plan. | The explicit lock path does not accept saved plans. |

configsctl logs each refusal. Each error identifies the workspace and its declared `lock_endpoints` and states the reason. A mutation without native endpoint metadata or declared lock-free resource types refuses the command.

A credential entry requires an endpoint for its `lock_host`. configsctl acquires and renews the credential workspace locks before preparing the deployment credential. After planning, the same lock set acquires any additional selected guest locks. A fresh scope covers the complete selected host and guest set through apply. The original credential lock remains active throughout preparation, planning, and apply.

In a workspace that declares `lock_endpoints`, configsctl runs OpenTofu help directly when an `apply` or `destroy` command includes `-help` or `-h`. For these requests, configsctl does not take a lock, read Ansible inventory, or run an Ansible program. The workspace does not need `tofu.guest_modules`. OpenTofu itself rejects `-help` combined with another positional argument, such as a saved plan, with exit status 1 and a message stating that the help topic does not exist.

The explicit lock path accepts only the following apply options. Each option with a value must use the `-name=value` form.

Accepted options:

| Option | Accepted form |
| --- | --- |
| `-auto-approve` | On the explicit lock path, configsctl accepts the bare option as true or accepts any of these values: `1`, `t`, `T`, `true`, `True`, `TRUE`, `0`, `f`, `F`, `false`, `False`, `FALSE`. configsctl rejects any other value with an invalid value error. A false value requires the same operator confirmation as an absent option. |
| `-no-color` | On the explicit lock path, configsctl accepts the bare option as true or accepts any of these values: `1`, `t`, `T`, `true`, `True`, `TRUE`, `0`, `f`, `F`, `false`, `False`, `FALSE`. configsctl rejects any other value with an invalid value error. The accepted value does not change command behavior. The plan command and the apply command always run with `-no-color`, even when the value is false. |
| `-input=false` | On the explicit lock path, configsctl accepts `-input` only with one of these false values: `0`, `f`, `F`, `false`, `False`, `FALSE`. configsctl rejects a bare `-input` and all other values with an error stating that the explicit lock path requires `-input=false`. |
| `-parallelism=N` | configsctl accepts a positive integer for `N` and defaults to `10`. |
| `-lock-timeout=DURATION` | configsctl accepts the value in the `-lock-timeout=DURATION` form. |

configsctl refuses every other option before OpenTofu starts, including these options:

- `-target`
- `-var`
- `-var-file`
- `-refresh=false`
- `-input=true`

The explicit lock path does not read the Ansible inventory and does not run any Ansible program. configsctl still reads the vault with its own code.

The explicit lock path runs these steps:

1. OpenTofu creates a plan with `-detailed-exitcode` in a private file. configsctl records the SHA-256 digest of the plan file immediately after planning.
2. OpenTofu shows the plan as JSON.
3. configsctl selects the hosts to lock from the declared `lock_targets` and endpoints. configsctl refuses a plan with unknown lock ownership before it takes any lock.
4. When the plan has changes and the effective arguments do not enable `-auto-approve`, configsctl requires confirmation. An argument `-auto-approve=false` does not enable automatic approval. configsctl treats input as disabled when either condition applies:

   1. The arguments include `-input` with an accepted false value.
   2. The arguments do not include an `-input` option, and `TF_INPUT` has the value `false` or `0`.

   When input is disabled and confirmation is required, configsctl refuses the apply before taking any lock and before OpenTofu applies. The error states that OpenTofu requires `-auto-approve` for a noninteractive apply with pending changes. When confirmation is required and input is enabled, configsctl asks the operator to enter `yes` on standard input. configsctl cancels the apply with the error "OpenTofu apply canceled" for any other answer.
5. configsctl acquires locks on all selected hosts.
6. configsctl compares the current SHA-256 digest of the plan file with the recorded digest and refuses to apply a changed file.
7. OpenTofu applies that exact plan.
8. configsctl releases the locks.

The explicit lock path passes only the following environment variables to OpenTofu. Each row states the source or condition for inclusion.

Environment variables:

| Variable | Value and condition |
| --- | --- |
| `PATH` | configsctl copies the value from its process environment. |
| `HOME` | configsctl sets the value to a new private directory and removes the directory after the command. |
| `TMPDIR` | configsctl sets the value to a new private directory for the plan file and plan JSON. configsctl removes the directory after the command. |
| `TF_IN_AUTOMATION` | configsctl sets the value to `1`. |
| `LANG` | configsctl passes the value only when its process environment sets it. |
| `TF_CLI_CONFIG_FILE` | configsctl passes this variable only when the configuration sets `tofu.provider_mirror`. configsctl sets its value to the generated provider mirror configuration path. |
| `TF_VAR_<name>` | configsctl passes a vault value when the vault key matches a variable that the workspace declares. configsctl redacts the value. |
| `<NAME>` | configsctl passes a vault value when the vault key uses `<env_key_prefix><NAME>`. configsctl redacts the value. |

When the configuration does not set `tofu.provider_mirror`, the explicit lock path does not pass `TF_CLI_CONFIG_FILE` or any native CLI configuration variable from the process environment.

The explicit lock path uses the saved workspace selection in the OpenTofu root, or `default` when the root does not store a workspace selection. The explicit lock path does not pass `TF_WORKSPACE` to OpenTofu. When the configsctl process sets `TF_WORKSPACE` to a value that differs from the selected workspace, configsctl refuses the apply before OpenTofu starts. configsctl logs the refusal and returns an error that states both values. The apply proceeds when the process does not set `TF_WORKSPACE` or sets it to the selected workspace.

The explicit apply fails when the vault stores an empty value for a key that the explicit lock path would pass. This rule covers keys that match declared workspace variable names or use the form `<env_key_prefix><NAME>`. The runner rejects empty secret values, and the error states that the environment does not set the secret. The other `configsctl tofu` commands export an empty value without an error.

The explicit lock path sets `HOME` to a private directory and passes only the environment variables in the table above. A provider that needs the real `HOME` or `SSH_AUTH_SOCK` does not work on the explicit lock path.

The automated controller tests for the explicit lock path run the real lock script and real OpenTofu but replace the ssh program with a local stand-in. The stand-in discards the ssh destination and runs the lock script on the test machine with one fixed lock directory for every lock host; the tests do not start an SSH server or open an SSH connection. These tests do not prove that the declared endpoint owns the lock or that the remote host uses the lock path and releases the lock. Acceptance with a real SSH endpoint must confirm endpoint ownership, lock path ownership, and release before adoption of the explicit lock path.

## Transfer existing OpenTofu state

Keep separate clean checkouts for the original configuration and the reviewed cutover configuration. Initialize cutover roots before inspection and keep original roots initialized for recovery. Retain every source and destination backend in the cutover checkout after removing transferred source resource declarations and adding destination declarations. Keep that checkout available throughout transfer and recovery.

Use absolute private-directory paths outside both checkouts, with existing parent directories. Each inspection requires a new directory; the transfer requires a separate directory. Do not force-push state or reset Git history to recover.

Migration commands that plan a workspace with a configured Dell credential adapter change the deployment account password. They generate one password per account, acquire an exclusive writer lease and the declared host locks, and activate that password for the command's provider checks. The password remains in memory. Supply required input values through `TF_VAR_` variables, protected vault bindings, or automatic variable files in the reviewed configuration commit.

1. From the cutover checkout root, inspect each source and destination:

   ```sh
   configsctl migrate-state inspect <selector> <inspection-directory>
   ```

   Inspection inventories contain backend identity and hashes of instance records rather than state values. Inspection retains encrypted snapshots for existing state and removes transient decrypted state.

   To diagnose an original root's native plan independently, run from the original checkout:

   ```sh
   configsctl migrate-state inspect-plan <selector> <inspection-directory>
   ```

   Plan inspection retains safe metadata reports and refuses pending managed changes or native errors. It does not require a manifest, create a journal or pause, write backend state, or rotate credentials.

2. Create a reviewed JSON manifest from those inspection inventories. The required fields are:

   | Field | Value |
   | --- | --- |
   | `version` | `1`. |
   | `commit` | Full Git commit SHA of the clean cutover configuration. |
   | `restore_commit` | Full Git commit SHA of the original configuration. |
   | `configuration_directory` | Absolute root of the retained cutover checkout, with clean HEAD equal to `commit`. |
   | `restore_configuration_directory` | Absolute root of the retained original checkout, with clean HEAD equal to `restore_commit`. |
   | `workspaces` | Inspected `selector`, `backend`, and `before` entries for every source and destination. |
   | `moves` | One entry per existing instance, including unchanged ownership, with `source`, `address`, `destination`, and `new_address`. |

   Duplicate ownership and incomplete mappings refuse preparation.

3. From the cutover checkout root, prepare encrypted before and after snapshots without writing deployment backends or pausing ordinary writes:

   ```sh
   configsctl migrate-state prepare <manifest> <private-directory>
   ```

4. Check prepared states before coordinating the transfer:

   ```sh
   configsctl migrate-state preflight <manifest> <private-directory>
   ```

   Add `--parallelism=N` with a positive integer to limit concurrent native plan operations. The option applies to every original and cutover plan in `preflight`, `transfer`, `execute`, `resume`, `verify`, and `verify-restored`. Omit it to use the OpenTofu default. Other migration commands reject the option.

   Preflight plans before snapshots against the original commit and after snapshots against the cutover commit in private local backends. Original plans use the original configuration's protected variables. Both configurations use the reviewed cutover provider mirror. Preflight compares complete managed changes after applying the reviewed address mapping. Existing pending actions may remain, but added, removed, or changed actions refuse the transfer. Unmapped creates and native validation errors also refuse the transfer. Output updates and data-source caches do not define managed changes.

   Preflight checks independent workspaces after a failure and retains private metadata reports with resource addresses, actions, changed attribute paths, and complete change hashes. Failed comparisons identify added, removed, or changed entries and their old and new hashes. Native error reports contain fixed diagnostic categories and configuration locations, with recognized HTTP statuses, numeric API error codes, or transport categories when available. Reports exclude values and diagnostic details. Preflight does not write deployment backends or create the durable pause.

5. Execute and verify the prepared transfer in one process:

   ```sh
   configsctl migrate-state transfer <manifest> <private-directory>
   ```

   `transfer` retains one executor, exclusive controller writer lease, and generated credential set across execution and verification. It compares the original and prepared plans before writing backend states, then checks every final backend and its plan before releasing the pause. Active writers prevent execution. The durable pause blocks new infrastructure mutations, service deployments, and credential refreshes until verification succeeds.

   After an interruption, retain the manifest and private directory. Run `transfer` again to resume execution and verification together, or use `resume` for execution followed by separate verification. Each command generates and activates a fresh deployment password for configured adapters.

6. After separate `execute` or `resume` stages, verify every final backend and compare its native plan to the recorded original plan before releasing the pause:

   ```sh
   configsctl migrate-state verify <manifest> <private-directory>
   ```

7. To reverse an unverified transfer instead, run recovery from the original clean checkout at `restore_commit`:

   ```sh
   configsctl migrate-state restore <manifest> <private-directory>
   configsctl migrate-state verify-restored <manifest> <private-directory>
   ```

   `restore` restores original ownership and retains the pause. `verify-restored` checks all retained backend states, including empty new destinations, and compares native plans from the original roots to the original baseline before releasing the pause. If forward preflight never succeeded, restoration verification derives the original baseline from the immutable before snapshots. Unexpected state or a changed original plan refuses verification.

## Rotate a Dell provider credential

A workspace can use an existing iDRAC rotator account to set a fresh password
for a separate deployment account. Store only the rotator password in the
Configs vault. The rotator needs Login and Configure Users. Configure Users
allows changes to every iDRAC account; iDRAC does not restrict it to the selected
deployment account.

```yaml
tofu:
  credentials:
    opentofu/firmware:
      lock_host: poweredge
      username_variable: poweredge_idrac_user
      password_variable: poweredge_idrac_password
      dell:
        endpoint: https://[<iDRAC IPv6 address>]
        ca_file: certificates/idrac.pem
        server_name: idrac.example.net
        rotator_user: tofu-rotator
        rotator_password_key: vault_idrac_rotator_password
        account_id: "3"
        username: tofu-firmware
        read_paths:
          - /redfish/v1/Systems/System.Embedded.1
```

Declare both selected OpenTofu variables with `sensitive = true` and
`ephemeral = true`. configsctl rejects other declarations. Each entry in
`read_paths` must accept authenticated GET requests needed by the provider.
The adapter verifies those reads with the rotator before changing the account,
then verifies them with the generated deployment credential.

The `plan`, `refresh`, `import`, `apply`, and `destroy` commands acquire
the selected host lock before rotating the deployment password. An apply also
uses the existing resource-based host selection. The lock remains active until
OpenTofu exits. The rotator password stays inside configsctl. Only the deployment
credential enters the OpenTofu child environment. configsctl redacts that
password from terminal output and run logs and refuses `TF_LOG_PATH`, which
would bypass its output redaction.

A native saved-plan apply generates a fresh password. The ephemeral username and
password do not persist in the saved plan or state. A failed password request or
failed login stops execution before OpenTofu starts. A later command can recover
with another rotation through the rotator account. The generated password stays
valid on iDRAC until the next rotation; command completion does not revoke it.
The `fmt`, `validate`, and `show` commands do not rotate credentials.

The live integration test requires an approved Configs checkout with the
`opentofu/firmware` credential entry and the existing PowerEdge lock target.
It changes that deployment account password and applies only a private local
`terraform_data` resource. It also reads the real NIC through the pinned REST
provider. Run it only during an available host lock window:

```sh
CONFIGSCTL_DELL_INTEGRATION_ROOT=/path/to/configs go test ./cmd/configsctl -run '^TestDellCredentialSavedPlanLifecycle$' -count=1 -v
```

## Run OpenTofu in CI

Run `configsctl ci tofu` from the configuration checkout root. The OpenTofu root must be inside that checkout. The command finds `tofu` in `PATH` and does not read the vault or its password file.

```sh
configsctl ci tofu --config <file> --root <root> --workspace <workspace> -- plan -input=false -no-color -detailed-exitcode -parallelism=<n> -out=<private-plan>
```

Set `TF_IN_AUTOMATION=1` and use private directories for `HOME` and `TMPDIR`. `plan` accepts `-input`, `-no-color`, `-detailed-exitcode`, `-parallelism`, `-out`, `-var`, `-var-file`, `-target`, `-replace`, `-destroy`, `-refresh`, `-lock`, and `-lock-timeout` and requires `-input=false`, `-no-color`, `-detailed-exitcode`, a positive `-parallelism`, and an `-out` path for a new file in a private directory below `TMPDIR`; `apply` accepts only `-input`, `-parallelism`, `-no-color`, and `-lock-timeout` and requires `-input=false` and a positive `-parallelism`. The command accepts `init`, `workspace select`, `plan`, `show`, `validate`, and `fmt -check`. It refuses `apply` because the CI command does not acquire host locks. Select the declared workspace before running `plan` or `show` without a saved plan. Use `-name=value` for flags with values. The command returns the OpenTofu exit status.

The configuration is a JSON object with these keys:

| Key | Value |
| --- | --- |
| `version` | `1`. |
| `required_environment` | Array of required environment variable names. |
| `secret_environment` | Array of environment variable names. `Run` redacts their values from console output and the private run log. Each value must contain at least 16 bytes. |
| `secret_files` | Object mapping environment variable names to absolute paths of private files. Each variable must contain its mapped path. |
| `output_mode` | `redacted_progress` or `private_json`. |
| `json_path` | Path to a new file in a private directory below `TMPDIR`. The configuration requires this key for `private_json` and forbids it for `redacted_progress`. |

The command rejects configurations with unknown or duplicate keys, missing required keys, null values, or trailing data. Keep secret values out of command arguments. `private_json` accepts only `show -json` with one saved plan and writes unredacted JSON to `json_path` with mode `0600`.

Go callers can use `goodkind.io/configsctl/tofurun` and call `Run(ctx, request, scope)` with a `tofurun.Request`. `Run` returns a `tofurun.Result` and an error. A saved-plan `apply` requires a `*hostlock.Scope` from `(*hostlock.Set).Scope`; the scope must cover every host in `Request.RequiredHosts`. `Scope.Check` verifies the scope and its required hosts.

## Prerequisites

- Go at the version in `go.mod`.
- A `python3` that can import `jinja2` in isolated mode for explicit template lint requests. On macOS,
  `/opt/homebrew/bin/python3` or `/usr/local/bin/python3` can provide it when
  the shell's `python3` cannot.
- OpenTofu for native validation and infrastructure commands.
- Git and access to `origin` for OpenTofu commands that write deployment state.
- The vault password at `~/.config/ansible/vault.pass` for every command that reads the vault.

## Develop

Lint, build, test, and release come from the shared go-makefile pipeline, so use the make targets rather than calling the Go tools directly.

1. Run `make build` to run the build and its required checks.
2. Run `make test` to run the tests.
3. Run `make help` to list every other target.
