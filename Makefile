# `make help` is the canonical source of truth for every target this repo
# supports. Run it before adding anything new. Lint, build, test, deadcode,
# release, baseline, and service-install all live in the central go-makefile
# pipeline fetched at parse time. Do NOT add project-local lint, deadcode,
# audit, fmt, vet, or staticcheck targets here. They duplicate the central
# pipeline and let agents bypass strict rules.

# Identity
BINARY := configsctl
CMD    := ./cmd/configsctl

# Pipeline modules. Add go-service.mk if this binary ships as a daemon and
# set LAUNCHD_LABEL, SYSTEMD_UNIT, LOG_PATH before the include bootstrap.mk line.
GO_MK_MODULES := go-build.mk go-release.mk

# Optional codegen hook. If this repo generates source before compiling (for
# example a tree-sitter parser or proto), set GO_MK_GENERATE to the codegen
# target name(s) here, before include bootstrap.mk. go.mk runs them as an
# order-only prerequisite of build, lint, vet, test, and govulncheck. Leave
# unset when there is no codegen. To earn docs-only CI skips, also set
# GO_MK_GENERATE_INPUTS to the repo-relative input dirs whose changes should
# force the gates.
# GO_MK_GENERATE := my-codegen-target
# GO_MK_GENERATE_INPUTS := proto api
# Optional go.work routing. If this repo vendors a module the proxy cannot build
# on its own (a submodule whose C sources are absent from its module zip), set
# GO_MK_WORKSPACE_USE to the workspace use-paths; go.mk materializes a gitignored
# go.work from them before every build. Leave unset otherwise.
# GO_MK_WORKSPACE_USE := . third_party/my-vendored-module
# bootstrap.mk fetches go.mk + golangci.yml + every module in GO_MK_MODULES
# at parse time and -includes them. Update path: edit go-makefile/bootstrap.mk,
# then refresh consumer copies (one-off cp; not enshrined as infrastructure).
include bootstrap.mk

.DEFAULT_GOAL := check
