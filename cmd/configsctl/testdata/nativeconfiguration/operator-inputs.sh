#!/usr/bin/env bash

set -euo pipefail

umask 077
if [ "${TF_VAR_not_declared+x}" = x ]; then
    echo 'The child received an undeclared Terraform input.' >&2
    exit 1
fi
if [ "${UNRELATED_CANARY+x}" = x ]; then
    echo 'The child received an unrelated ambient variable.' >&2
    exit 1
fi
printf '%s\n' "${HOME:?}" "${SSH_AUTH_SOCK:?}" "${OPERATOR_PROFILE:?}" \
    "${OPERATOR_VALUE:?}" "${OPERATOR_EMPTY?}" "${OPERATOR_TRANSIENT:?}" \
    > "${OPERATOR_RESULT:?}"
if ssh-add -l > "${OPERATOR_RESULT:?}.agent" 2>&1; then
    echo 'The fixture agent unexpectedly contains an identity.' >&2
    exit 1
else
    AGENT_STATUS=$?
    if [ "$AGENT_STATUS" -ne 1 ]; then
        cat "${OPERATOR_RESULT:?}.agent" >&2
        exit "$AGENT_STATUS"
    fi
fi
printf '%s\n' "${OPERATOR_TRANSIENT:?}"
