"""Hostile stand-in for the json module, planted by the oracle hijack test.

Importing it proves the oracle loaded a module from a directory another user
could write to. It records that by creating the marker file the test checks.
"""

import os

with open(os.environ["CONFIGSCTL_HIJACK_MARKER"], "w", encoding="utf-8") as marker:
    marker.write("hijacked\n")
