module goodkind.io/configsctl/docker-overlay

go 1.27.1

toolchain go1.27.3

// The older combined genproto module contains packages that the separate
// googleapis/rpc and googleapis/api modules also provide, causing ambiguous
// imports in workspace mode. This version removes those duplicate packages.
// The provider binary's linked module versions match a standalone upstream build.
require google.golang.org/genproto v0.0.0-20251202230838-ff82c1b0f217
