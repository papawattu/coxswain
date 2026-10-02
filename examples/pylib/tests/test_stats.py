"""Acceptance tests for numutils (protected path, preset none).

The seed state fails here: ``Median`` is not defined by the package.
Task 1 (tasks.md) adds it; task 2 adds ``Clamp`` (tests/test_clamp.py).
"""

import unittest

from numutils import mean, mode


class TestMeanMode(unittest.TestCase):
    def test_mean(self):
        self.assertEqual(mean([1, 2, 3]), 2.0)

    def test_mean_empty_raises(self):
        with self.assertRaises(ValueError):
            mean([])

    def test_mode(self):
        self.assertEqual(mode([1, 2, 2, 3]), 2)


if __name__ == "__main__":
    unittest.main()
