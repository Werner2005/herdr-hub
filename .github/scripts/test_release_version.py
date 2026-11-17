import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location(
    "release_version", Path(__file__).with_name("release-version.py")
)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class ReleaseVersionTests(unittest.TestCase):
    def test_initial_release(self):
        self.assertEqual(module.release_version("v0.1"), "0.1.0")

    def test_invalid_or_mismatched_versions(self):
        for tag in ("main", "0.1", "v0.1;echo bad", "v0.1.0-rc1", "v0.2", "v0.1.1", "v0.1.0"):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                module.release_version(tag)


if __name__ == "__main__":
    unittest.main()
