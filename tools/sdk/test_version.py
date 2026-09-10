"""Calendar boundaries and irreversible reservation safety for coordinated releases."""
from datetime import datetime, timedelta, timezone
import unittest

from version import canonical_reservation, next_release, package_versions


class CalendarVersionTest(unittest.TestCase):
    def test_one_release_maps_to_all_registry_coordinates(self):
        self.assertEqual(package_versions("2026.9.2"), {
            "python": "2026.9.2", "typescript": "2026.9.2", "java": "2026.9.2", "go": "1.202609.2",
        })
        self.assertEqual(canonical_reservation("v1.202701.1"), "2027.1.1")

    def test_unmerged_candidate_is_stable_and_rolls_over_without_reusing_reservations(self):
        september = datetime(2026, 9, 8, tzinfo=timezone.utc)
        self.assertEqual(next_release(september, [], None), "2026.9.1")
        self.assertEqual(next_release(september, ["2026.9.1"], None), "2026.9.2")
        self.assertEqual(next_release(september, ["2026.9.1"], "2026.9.2"), "2026.9.2")
        self.assertEqual(next_release(datetime(2026, 10, 1, tzinfo=timezone.utc), ["2026.9.1"], "2026.9.2"), "2026.10.1")
        self.assertEqual(next_release(datetime(2027, 1, 1, tzinfo=timezone.utc), ["v1.202612.4"], None), "2027.1.1")

    def test_sequences_are_numeric_and_go_publications_reserve_canonical_numbers(self):
        self.assertEqual(next_release(datetime(2026, 9, 8, tzinfo=timezone.utc), ["2026.9.9", "v1.202609.10"], None), "2026.9.11")

    def test_allocation_uses_utc_rather_than_the_clocks_local_month(self):
        local = datetime(2026, 10, 1, 0, 30, tzinfo=timezone(timedelta(hours=2)))
        self.assertEqual(next_release(local, ["2026.9.1"], None), "2026.9.2")

    def test_invalid_ids_and_mapping_shaped_ids_are_not_canonical(self):
        for invalid in ["2026.09.1", "2026.9.01", "2026.0.1", "2026.13.1", "2026.9.0", "2026.9.1.1", "2026.9.1-rc1", "1.202609.1", "2026.9.1\n"]:
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                package_versions(invalid)

    def test_clock_regression_and_ownership_conflicts_halt(self):
        now = datetime(2026, 9, 8, tzinfo=timezone.utc)
        with self.assertRaisesRegex(ValueError, "timezone-aware"):
            next_release(now.replace(tzinfo=None), [], None)
        with self.assertRaisesRegex(ValueError, "precedes"):
            next_release(now, ["2026.10.1"], None)
        with self.assertRaisesRegex(ValueError, "collides"):
            next_release(now, ["v1.202609.2"], "2026.9.2")
        with self.assertRaisesRegex(ValueError, "precedes"):
            next_release(now, ["2026.9.3"], "2026.9.2")


if __name__ == "__main__":
    unittest.main()
