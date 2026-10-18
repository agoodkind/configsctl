# configsctl

configsctl is the control tool for the configs repository. It runs Ansible deploys, lints Ansible input variables, reads and updates the Ansible vault, and runs OpenTofu with credentials from the vault.

## Run it from a configs checkout

configsctl resolves every path it uses relative to its working directory, so it only works from the root of a configs checkout. From there it finds the Ansible tree, the vault, the OpenTofu tree, and the lint baseline. The jinja2 lint oracle ships inside the binary. Run from anywhere else, it finds none of them.

To run a deploy from the root of a configs checkout:

```sh
go run goodkind.io/configsctl/cmd/configsctl@latest deploy <playbook>
```

The commands are `lint`, `baseline`, `keys`, `secret`, `set-secrets`, `rename-secret`, `delete-secrets`, `deploy`, `tofu`, `syntax-check`, `inventory-dump`, and `version`. `version` prints the commit the binary was built from, whether that tree was dirty, and a hash of the binary. A deploy lints the files its playbook reads and refuses to run on a new finding. The playbooks pin and pull the releases they install; configsctl stages nothing.

## Select an OpenTofu workspace

A workspace is a directory with a `backend` block inside a `terraform` block in one of its `*.tf` or `*.tf.json` files. The workspace name is the directory name. configsctl checks the directory in `tofu.workspaces_dir` and each direct child directory of it. A directory without a `backend` block is a module, not a workspace.

`configsctl.yml` at the root of the configs checkout has two required keys:

```yaml
tofu:
  workspaces_dir: opentofu
  env_key_prefix: tofu_env_
```

- `workspaces_dir` is the path of the directory with the workspaces, relative to the root of the checkout.
- `env_key_prefix` selects the vault keys that configsctl exports as plain environment variables. A vault key `<env_key_prefix><NAME>` exports as `<NAME>`.

To run OpenTofu in a child workspace, pass the workspace name as the first argument:

```sh
configsctl tofu <workspace> plan
```

To run OpenTofu in `workspaces_dir` itself, omit the workspace name:

```sh
configsctl tofu plan
```

configsctl passes every argument after the workspace name to OpenTofu. When the first argument is not a child workspace name, configsctl passes every argument to OpenTofu. When `workspaces_dir` has no `backend` block and the first argument is not a child workspace name, the command fails and prints the child workspace names.

configsctl exports a vault key as `TF_VAR_<key>` only when the selected workspace declares a variable with that name.

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

``sh
CONFIGSCTL_DELL_INTEGRATION_ROOT=/path/to/configs go test ./cmd/configsctl -run '^TestDellCredentialSavedPlanLifecycle$' -count=1 -v
``sh

## Gate a deploy on the guest plan

configsctl runs targeted OpenTofu plans before acquiring host locks or running Ansible when `tofu.guest_module` is set. The key selects the module address in the `guest` workspace and requires that workspace's `guest_hosts` mapping under `tofu.lock_targets`. The gate plans only selected playbook hosts listed as values in that mapping. Each plan targets the module instance keyed by the host's `service_name` inventory variable. The gate refuses the deploy if any plan fails or reports pending changes.

The `--check` option skips the gate. An absent `tofu.guest_module` key disables the gate.

```yaml
tofu:
  guest_module: module.base
  lock_targets:
    opentofu/guest:
      guest_hosts:
        suburban/224: clyde.suburban.goodkind.io
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

1. Run `make check` to run every lint gate.
2. Run `make test` to run the tests.
3. Run `make help` to list every other target.
