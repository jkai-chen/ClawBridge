#!/usr/bin/env python3
"""Prints a go test -run pattern for one deterministic shard of a package's tests.

CI splits the packages whose race run is too long for one job by test name:
  go test -race -list . ./internal/bus | python3 scripts/go_test_shard.py 1 3
reads the listing on stdin and prints ^(TestA|TestB|...)$ for shard 1 of 3.
Top-level tests, examples and fuzz targets are sorted by name and dealt out
in turn, so the shards are disjoint, together cover every test, and are the
same on every run. Benchmarks don't run under -run and are left out.
"""
import re
import sys

RUNNABLE = re.compile(r'^(Test|Example|Fuzz)\w*$')


def names(listing):
    found = sorted({line.strip() for line in listing.splitlines() if RUNNABLE.match(line.strip())})
    if not found:
        raise SystemExit('go_test_shard: the listing has no tests; was go test -list run?')
    return found


def shard(found, index, count):
    if not 1 <= index <= count:
        raise SystemExit(f'go_test_shard: shard {index} of {count} is out of range')
    return [n for i, n in enumerate(found) if i % count == index - 1]


def pattern(chosen):
    # An empty shard (more shards than tests) runs nothing.
    return '^(' + '|'.join(chosen) + ')$' if chosen else '^$'


def main(argv):
    if len(argv) != 3:
        raise SystemExit('usage: go test -list . PKG | go_test_shard.py INDEX COUNT')
    print(pattern(shard(names(sys.stdin.read()), int(argv[1]), int(argv[2]))))


if __name__ == '__main__':
    main(sys.argv)
