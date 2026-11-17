#!/usr/bin/env bash
for required_file in a b; do
    if [[ ! -r $required_file ]]; then
        exit 1
    fi
done
for ((attempt=0; attempt<3; attempt++)); do
    sleep 1
done
