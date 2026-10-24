# configsctl

configsctl is the control tool for the configs repository. It runs Ansible deploys, lints Ansible input variables, reads and updates the Ansible vault, and runs OpenTofu with credentials from the vault.

## Run it from a configs checkout

configsctl resolves every path it uses relative to its working directory, so it only works from the root of a configs checkout. From there it finds the Ansible tree, the vault, the OpenTofu tree, and the lint baseline. The jinja2 lint oracle ships inside the binary. Run from anywhere else, it finds none of them.

To run a deploy from the root of a configs checkout:

```sh
go run goodkind.io/configsctl/cmd/configsctl@latest deploy <playbook>
```

The commands are `lint`, `baseline`, `keys`, `secret`, `set-secrets`, `rename-secret`, `delete-secrets`, `deploy`, `tofu`, `migrate-state`, `syntax-check`, `inventory-dump`, and `version`. `version` prints the commit the binary was built from, whether that tree was dirty, and a hash of the binary. A deploy lints the files its playbook reads and refuses to run on a new finding. The playbooks pin and pull the releases they install; configsctl stages nothing.

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
setting is present. Without this setting, configsctl preserves the native CLI
configuration environment.

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

## Scope infrastructure locks

When `tofu.guest_modules` enables independent workspaces, an ordinary `apply` or `destroy` prepares a native saved plan before selecting locks. configsctl preserves the confirmation prompt unless the effective arguments include `-auto-approve`. It applies that exact plan after acquiring its declared locks. Unknown ownership refuses the mutation.

Guest-local changes use the enrolled guest's existing SSH lock. A known node and numeric VMID without deployment enrollment use a guest-specific lock directory through the declared hypervisor's SSH connection. Explicit resource ownership selects the host lock for guest creation, shared host changes, and firmware. `lock_free_types` can define a root that does not need Proxmox locks.

configsctl expands `TF_CLI_ARGS` and command-specific arguments once, using OpenTofu's argument order. Explicit arguments take precedence over environment arguments.

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

A saved-plan apply generates a fresh password. The ephemeral username and
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

## Gate a deploy on the guest plan

configsctl checks each selected enrolled guest in its owning workspace before acquiring deployment locks or running Ansible. `tofu.guest_modules` maps workspace selectors to the module address indexed by the inventory's `service_name`. Each workspace's `guest_hosts` mapping supplies its enrolled inventory hosts. Duplicate ownership and missing required mappings refuse deployment.

Each plan targets the selected guest module in its root. The gate verifies that the saved plan includes that exact module instance. A failed plan, pending changes, or an empty target plan refuses deployment. Unrelated workspace configuration and state do not participate in the check.

The `--check` option skips the gate. A configuration without either guest-module key disables it. Legacy `tofu.guest_module` still selects the shared `guest` workspace during tooling rollout; the configuration cannot set both keys.

```yaml
tofu:
  guest_modules:
    suburban/tack-qa: module.base
  lock_targets:
    opentofu/suburban/tack-qa:
      guest_hosts:
        suburban/217: tack-qa.suburban.goodkind.io
```

## Deploy behavior for hosts without Proxmox guests

The `deploy.absent_guest_nodes` setting maps the second label of a host name to a Proxmox node name from `ansible_proxmox_host`. A missing or empty mapping disables the check.

```yaml
deploy:
  absent_guest_nodes:
    home: vault
```

This mapping checks hosts such as `tack-search1.home.goodkind.io` against the Proxmox node `vault`. A play host qualifies for the check only when it has the inventory variable `service_vmid` and its second label appears in the mapping.

A guest matches when an inventory host has `ansible_proxmox_host` equal to the mapped node and `ansible_proxmox_vmid` equal to the play host's `service_vmid`. The check compares VMIDs rather than host names. The Proxmox inventory can list an existing guest under a different name, such as `mwan-failover` for `mwan-failover.home.goodkind.io`.

configsctl skips each checked play host without a matching guest. It does not lock that host. It adds an exclusion for the host to the Ansible limit, using a comma separator when the limit contains a comma and a colon separator otherwise.

The run log records `Skipping hosts with no Proxmox guest: <hosts>`. configsctl also logs `deploy.hosts_absent`.

With a nonempty mapping, the deploy fails before taking any locks when any of these conditions applies:

* The Proxmox inventory does not report any guests on the mapped node of a checked host.
* A skipped host is a value of `tofu.guest_hosts`.
* The check would skip every play host.
* The `--limit` value starts with `@`.

The check also runs with `--check`. Check mode now reads the settings and still does not take locks.

## Prerequisites

- Go at the version in `go.mod`.
- A `python3` that can import `jinja2` in isolated mode. On macOS,
  `/opt/homebrew/bin/python3` or `/usr/local/bin/python3` can provide it when
  the shell's `python3` cannot.
- The Ansible command line tools for `deploy`, `syntax-check`, and `inventory-dump`, and OpenTofu for `tofu`.
- The vault password at `~/.config/ansible/vault.pass` for every command that reads the vault.

## Develop

Lint, build, test, and release come from the shared go-makefile pipeline, so use the make targets rather than calling the Go tools directly.

1. Run `make build` to run the build and its required checks.
2. Run `make test` to run the tests.
3. Run `make help` to list every other target.
