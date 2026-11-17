import importlib.util
from pathlib import Path
import re
import unittest

spec = importlib.util.spec_from_file_location(
    "release_version", Path(__file__).with_name("release-version.py")
)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class ReleaseVersionTests(unittest.TestCase):
    def test_current_release(self):
        root = Path(__file__).resolve().parents[2]
        values = (root / "charts/herdr-hub/values.yaml").read_text()
        tag = re.search(r"workspace:(v[0-9.]+)\n", values)[1]
        expected = tag[1:] + (".0" if tag.count(".") == 1 else "")
        self.assertEqual(module.release_version(tag), expected)

    def test_invalid_or_mismatched_versions(self):
        for tag in ("main", "0.1", "v0.1;echo bad", "v0.1.0-rc1", "v999999.0"):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                module.release_version(tag)


if __name__ == "__main__":
    unittest.main()
