# The shared go-makefile pipeline builds the pinned docker provider v4.6.0
# source with extra-hosts.patch. Its generate hook writes src, then go-makefile
# creates a gitignored go.work using . and ./src. CMD selects the upstream main
# package, which the build compiles without a wrapper.
BINARY := terraform-provider-docker
CMD    := github.com/terraform-providers/terraform-provider-docker

DOCKER_PROVIDER_REPOSITORY := https://github.com/kreuzwerker/terraform-provider-docker.git
DOCKER_PROVIDER_VERSION    := v4.6.0
DOCKER_PROVIDER_REVISION   := 1e48613cd36acab30e9146aae6f036ea8e40a288
DOCKER_PROVIDER_PATCH      := extra-hosts.patch
DOCKER_PROVIDER_SOURCE     := src

# The overlay version differs from stock 4.6.0 so mirrors do not store the
# patched binary under the stock version and checksum. The release tag combines
# the binary name, overlay version, and short commit. The release workflow passes
# its computed tag in RELEASE_TAG; ?= preserves it. The root release uses a
# timestamp tag. A second release for the same commit fails at publish because
# the tag already has a release.
DOCKER_OVERLAY_VERSION := 4.6.0-configsctl.1
RELEASE_TAG            ?= $(BINARY)-v$(DOCKER_OVERLAY_VERSION)-$(GIT_COMMIT)
export RELEASE_TAG

# Upstream v4.6.0 builds Linux binaries without cgo and darwin binaries with cgo.
# On darwin, Docker Compose uses github.com/fsnotify/fsevents, which watches
# files with cgo code. The overlay CI and release jobs set darwin_runner and
# build darwin binaries on macOS, where the toolchain compiles that cgo code.
RELEASE_BINS :=$(BINARY):$(CMD):platforms=linux/amd64,platforms=linux/arm64 \
	$(BINARY):$(CMD):cgo=1,platforms=darwin/amd64,platforms=darwin/arm64

# The explicit file list gives gocyclo the overlay's Go file and excludes src.
# Without file names, gocyclo prints usage text that the gate reports as
# findings. The go-makefile default excludes findings in _test.go files.
GO_TEST_TARGETS := -timeout=30m ./...
GOCYCLO_TARGETS := $(wildcard *.go)

GO_MK_MODULES         := go-build.mk go-release.mk
GO_MK_GENERATE        := docker-provider-source
# GO_MK_GENERATE_INPUTS uses repository-root paths. This path makes the shared
# CI change detector run overlay gates for changes anywhere under docker-overlay,
# including the patch and OpenTofu test fixtures.
GO_MK_GENERATE_INPUTS := docker-overlay
GO_MK_WORKSPACE_USE   := . ./$(DOCKER_PROVIDER_SOURCE)

include bootstrap.mk

# Each build, test, lint, and compile-stage release run fetches src again.
# The recipe requires the v4.6.0 tag, pinned revision, and fetched commit to
# match. git apply stops the build if the source does not match the patch.
.PHONY: docker-provider-source
docker-provider-source:
	rm -rf $(DOCKER_PROVIDER_SOURCE)
	test "$$(git ls-remote --tags $(DOCKER_PROVIDER_REPOSITORY) refs/tags/$(DOCKER_PROVIDER_VERSION) | cut -f1)" = "$(DOCKER_PROVIDER_REVISION)"
	git init -q $(DOCKER_PROVIDER_SOURCE)
	git -C $(DOCKER_PROVIDER_SOURCE) fetch -q --depth 1 $(DOCKER_PROVIDER_REPOSITORY) $(DOCKER_PROVIDER_REVISION)
	git -C $(DOCKER_PROVIDER_SOURCE) checkout -q --detach FETCH_HEAD
	test "$$(git -C $(DOCKER_PROVIDER_SOURCE) rev-parse HEAD)" = "$(DOCKER_PROVIDER_REVISION)"
	git -C $(DOCKER_PROVIDER_SOURCE) apply ../$(DOCKER_PROVIDER_PATCH)
