# configsctl

configsctl is the control tool for the configs repository. It runs Ansible deploys, stages published mwan and opnsensectl releases for those deploys, lints Ansible input variables, reads and updates the Ansible vault, and runs OpenTofu with credentials from the vault. The repository also builds deploy-authorized-keys, which writes SSH authorization bundles from GitHub keys.

## Run it from a configs checkout

configsctl resolves every path it uses relative to its working directory, so it only works from the root of a configs checkout. From there it finds the Ansible tree, the vault, the OpenTofu tree, the lint baseline, and the release cache under `.make/releases`. The jinja2 lint oracle ships inside the binary. Run from anywhere else, it finds none of them.

To run a deploy from the root of a configs checkout:

```sh
go run goodkind.io/configsctl/cmd/configsctl@latest deploy <playbook>
```

The commands are `lint`, `baseline`, `keys`, `secret`, `set-secrets`, `deploy`, `tofu`, `syntax-check`, `inventory-dump`, and `version`. `version` prints the commit the binary was built from, whether that tree was dirty, and a hash of the binary. A deploy lints the files its playbook reads and refuses to run on a new finding. `deploy --release <tag>` downloads and verifies that mwan release before the play starts, and `deploy --opnsensectl-release <tag>` does the same for an opnsensectl release.

## Prerequisites

- Go at the version in `go.mod`.
- python3 with jinja2, which the lint gate needs for expressions its Go parser cannot read.
- The Ansible command line tools for `deploy`, `syntax-check`, and `inventory-dump`, and OpenTofu for `tofu`.
- The vault password at `~/.config/ansible/vault.pass` for every command that reads the vault.
- An authenticated gh CLI or a `GITHUB_TOKEN` is optional. Release staging works anonymously at a lower GitHub rate limit.

## Develop

Lint, build, test, and release come from the shared go-makefile pipeline, so use the make targets rather than calling the Go tools directly.

1. Run `make check` to run every lint gate.
2. Run `make test` to run the tests.
3. Run `make help` to list every other target.
