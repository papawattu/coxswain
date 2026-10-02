"""Acceptance tests for Clamp (protected path, preset none).

Red in the seed state too: Clamp arrives with task 2 (tasks.md), applied on
top of the task 1 state.
"""

import unittest

from numutils import Clamp


class TestClamp(unittest.TestCase):
    def test_inside_range_is_identity(self):
        self.assertEqual(Clamp(5, 0, 10), 5)

    def test_below_range(self):
        self.assertEqual(Clamp(-1, 0, 10), 0)

    def test_above_range(self):
        self.assertEqual(Clamp(11, 0, 10), 10)

    def test_float_range(self):
        self.assertEqual(Clamp(0.2, 0.1, 0.9), 0.2)
        self.assertEqual(Clamp(0.0, 0.1, 0.9), 0.1)


if __name__ == "__main__":
    unittest.main()
