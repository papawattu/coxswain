"""Acceptance tests for Median (protected path, preset none).

This module makes the seed state red: it imports a function the package
does not define yet. Task 1 (tasks.md) implements Median.
"""

import unittest

from numutils import Median


class TestMedian(unittest.TestCase):
    def test_odd_count(self):
        self.assertEqual(Median([3, 1, 2]), 2)

    def test_even_count_averages_middle_pair(self):
        self.assertEqual(Median([4, 1, 3, 2]), 2.5)

    def test_single_value(self):
        self.assertEqual(Median([7]), 7)

    def test_does_not_mutate_input(self):
        data = [5, 1, 3]
        Median(data)
        self.assertEqual(data, [5, 1, 3])

    def test_empty_raises(self):
        with self.assertRaises(ValueError):
            Median([])


if __name__ == "__main__":
    unittest.main()
