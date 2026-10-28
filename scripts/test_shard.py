#!/usr/bin/env python3
"""The CI test shards are disjoint, cover every test, and don't change between runs."""
from pathlib import Path
import re
import subprocess
import sys
import unittest

SCRIPT = Path(__file__).resolve().parent / 'go_test_shard.py'
LISTING = '\n'.join(['TestB', 'TestA', 'ExampleRoles', 'FuzzParse', 'BenchmarkSlow', 'TestC', 'TestA', 'TestD',
                     'TestE', 'ok  \tclawbridge/internal/bus\t0.01s', ''])
RUNNABLE = ['ExampleRoles', 'FuzzParse', 'TestA', 'TestB', 'TestC', 'TestD', 'TestE']


def run(index, count, listing=LISTING):
    return subprocess.run([sys.executable, str(SCRIPT), str(index), str(count)], input=listing,
                          capture_output=True, text=True)


def parse(out):
    m = re.fullmatch(r'\^\((.*)\)\$\n', out)
    return m.group(1).split('|') if m else []


class ShardTest(unittest.TestCase):
    def test_shards_partition_every_runnable_test(self):
        for count in (1, 2, 3, 4):
            seen = []
            for index in range(1, count + 1):
                r = run(index, count)
                self.assertEqual(r.returncode, 0, r.stderr)
                seen += parse(r.stdout)
            self.assertEqual(sorted(seen), RUNNABLE)
            self.assertEqual(len(seen), len(set(seen)))

    def test_shards_are_stable_and_sorted(self):
        shuffled = '\n'.join(reversed(LISTING.split('\n')))
        self.assertEqual(run(2, 3).stdout, run(2, 3, shuffled).stdout)
        self.assertEqual(parse(run(1, 3).stdout), ['ExampleRoles', 'TestB', 'TestE'])

    def test_refusals(self):
        self.assertNotEqual(run(0, 3).returncode, 0)
        self.assertNotEqual(run(4, 3).returncode, 0)
        self.assertNotEqual(run(1, 2, 'ok  \tclawbridge/x\t0.01s\n').returncode, 0)
        self.assertEqual(run(8, 9).stdout, '^$\n')


if __name__ == '__main__':
    unittest.main()
