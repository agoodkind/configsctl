#!/usr/bin/env bash
for file in a b; do
    install -m 0644 "$file" /tmp/
done
