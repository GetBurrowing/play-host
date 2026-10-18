#!/bin/sh
# Fail on any tracked file that names a particular install, person or private
# host. The denylist is stored hashed; see scripts/scrub_check.py for how to
# add a term. Usage: scripts/scrub-check.sh [repo-dir]
set -eu
exec python3 "$(dirname "$0")/scrub_check.py" "${1:-.}"
