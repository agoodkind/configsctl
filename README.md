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
