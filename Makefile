# `make help` is the canonical source of truth for every target this repo
# supports. Run it before adding anything new. Lint, build, test, deadcode,
# release, baseline, and service-install all live in the central go-makefile
# pipeline fetched at parse time. Do NOT add project-local lint, deadcode,
# audit, fmt, vet, or staticcheck targets here. They duplicate the central
# pipeline and let agents bypass strict rules.

# Identity
BINARY := configs
CMD    := ./cmd/configs

# Pipeline modules. Add go-service.mk if this binary ships as a daemon and
# set LAUNCHD_LABEL, SYSTEMD_UNIT, LOG_PATH before -include $(GO_MK).
GO_MK_MODULES := go-build.mk go-release.mk

# bootstrap.mk fetches go.mk + golangci.yml + every module in GO_MK_MODULES
# at parse time and -includes them. Update path: edit go-makefile/bootstrap.mk,
# then refresh consumer copies (one-off cp; not enshrined as infrastructure).
include bootstrap.mk

.DEFAULT_GOAL := check
