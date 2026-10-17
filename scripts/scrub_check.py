#!/usr/bin/env python3
"""Fail if any tracked file names a particular install, person or private host.

This repository is public, so the denylist must not spell out the terms it
guards against: a plaintext list of names would itself be the leak. The terms
are stored as sha256 hex digests of their lowercase spelling, and the scan
hashes candidate tokens from each tracked file and compares digests.

Matching rules (all case-insensitive):

* SUBSTRING_HASHES: a term that is caught anywhere inside a word, for
  distinctive names that never occur inside an ordinary English word.
* WORD_HASHES: a term that is caught only as a whole word. A word is a
  maximal run of [a-z0-9]; everything else (spaces, punctuation, "_", "-",
  ".") separates words.
* COMPOUND_HASHES: a dotted or hyphenated name such as a domain, caught when
  it appears as a contiguous run of two or more words joined by "." or "-"
  (so it is also caught as the tail of a longer host name).
* PLAIN_PATTERNS: generic regexes that identify nothing on their own (a
  private address range, an email domain outside the reserved example names). They may stay plaintext.

A hit prints file:line and the matched token. The token is already in the
scanned file, so printing it discloses nothing the file does not.

To add a term, compute its digest and add it to the right set:

    printf %s 'theterm' | sha256sum

Use the lowercase spelling, no trailing newline. Do not write the term itself
in this file, in a commit message, or in a comment.

Excluded from the scan: go.sum, LICENSE, NOTICE, and any file that is not
UTF-8 text.
"""

from __future__ import annotations

import hashlib
import re
import subprocess
import sys
from pathlib import Path

SUBSTRING_HASHES = {
    "9cfdc719f125bfd24830658f109d785ecca57a0c05022add0f6cf4aaec5dc25c",
    "44506f784f8d79f5c01259f925b02d157fc5703fa334313d7575d43f308c9a36",
    "34d45d6102463d492be4bfd1aa40ea8d998a08fab131469fc8abc9b57c365ff1",
    "8ac6353870ef2a9553cba93548c3c93dd4d8615fe119f45afc78a20e656d9ce7",
    "faa10b64ff114994f7f2a6ef22d4c916be47ac2a2f86f8698072990815724e25",
    "4971d10a528354c86e36d90b17472230c87a26376491b1005483d5dd29a9d5d0",
    "6234c4dc4560ecd116b9f4f40fe497217e92ee7bb60ca0f3ecddc4f8b079c483",
    "c39b1c7a7d02f4ef65ee6fbaa87e27f42c6c8ec31d6b3423b1177aedd1aed299",
}

WORD_HASHES = {
    "e0e3a2b6471d044a53a7757994b51dd33c6b3ec90e1aca21cebc8e2ae79d6d9b",
}

COMPOUND_HASHES = {
    "7bee97866a16719e2f6eca472a6f171e04da80ae3d550052f2747286d1d2fd76",
    "b29a46a27acbba77b0aaba113e3d343cbd951a3a848f8356c164c3728b1c8e05",
    "b185c3068c145f728a1aab6f00091fb6735ddf72311b368f592cb7b0a2a5f37c",
    "5dea22a579e9477122312d9f7269eabfa8147527a8d24738683f6b6734487560",
}

# Substrings of every word are hashed at these lengths. Widen the range if a
# term is ever added that is shorter or longer.
SUBSTRING_MIN, SUBSTRING_MAX = 3, 16

# Domains an address may use without naming anyone: the reserved example and
# test names, and code hosts whose "git@" SSH form looks like an address.
EXAMPLE_DOMAINS = r"(?:example|test|invalid|localhost|github|gitlab)"
PLAIN_PATTERNS = [
    re.compile(r"192\.168\."),
    re.compile(
        r"@(?!(?:[a-z0-9-]+\.)*" + EXAMPLE_DOMAINS + r"\.)[a-z0-9.-]+\.(?:com|technology)\b",
        re.IGNORECASE,
    ),
]

EXCLUDED_NAMES = {"go.sum", "LICENSE", "NOTICE"}

WORD_RE = re.compile(r"[a-z0-9]+")
COMPOUND_RE = re.compile(r"[a-z0-9]+(?:[.-][a-z0-9]+)+")


def _digest(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def tokens_hit(line: str) -> list[str]:
    """Return the tokens on this line whose digests are on the denylist."""
    lower = line.lower()
    found: list[str] = []
    for m in WORD_RE.finditer(lower):
        word = m.group(0)
        if _digest(word) in WORD_HASHES:
            found.append(word)
            continue
        hit = None
        for size in range(SUBSTRING_MIN, min(SUBSTRING_MAX, len(word)) + 1):
            for start in range(len(word) - size + 1):
                if _digest(word[start : start + size]) in SUBSTRING_HASHES:
                    hit = word
                    break
            if hit:
                break
        if hit:
            found.append(hit)
    for m in COMPOUND_RE.finditer(lower):
        parts = re.split(r"([.-])", m.group(0))
        words, seps = parts[0::2], parts[1::2]
        for i in range(len(words)):
            joined = words[i]
            for j in range(i + 1, len(words)):
                joined += seps[j - 1] + words[j]
                if _digest(joined) in COMPOUND_HASHES:
                    found.append(joined)
    for pattern in PLAIN_PATTERNS:
        found.extend(m.group(0) for m in pattern.finditer(line))
    return found


def tracked_files(root: Path) -> list[Path]:
    out = subprocess.run(
        ["git", "ls-files", "-z"], cwd=root, check=True, capture_output=True
    ).stdout
    return [root / n for n in out.decode().split("\0") if n]


def scan(root: Path) -> list[str]:
    hits = []
    for path in tracked_files(root):
        if path.name in EXCLUDED_NAMES or not path.is_file():
            continue
        data = path.read_bytes()
        if b"\0" in data:
            continue
        try:
            text = data.decode("utf-8")
        except UnicodeDecodeError:
            continue
        rel = path.relative_to(root).as_posix()
        for lineno, line in enumerate(text.splitlines(), 1):
            for token in tokens_hit(line):
                hits.append(f"{rel}:{lineno}: {token}")
    return hits


def main(argv: list[str]) -> int:
    root = Path(argv[1]) if len(argv) > 1 else Path.cwd()
    root = Path(
        subprocess.run(
            ["git", "rev-parse", "--show-toplevel"],
            cwd=root,
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()
    )
    files = tracked_files(root)
    if not files:
        print("scrub-check: git ls-files returned nothing; refusing to pass", file=sys.stderr)
        return 2
    hits = scan(root)
    for hit in hits:
        print(hit)
    if hits:
        print(f"scrub-check: {len(hits)} hit(s)", file=sys.stderr)
        return 1
    print(f"scrub-check: OK ({len(files)} tracked files)")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
