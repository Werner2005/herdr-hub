"""Render the actual chart to guard workload exec permissions and opt-in scope."""
import json
from pathlib import Path
import re
import shutil
import subprocess
import unittest

ROOT = Path(__file__).resolve().parents[2]


@unittest.skipUnless(shutil.which("helm"), "Helm required (installed by chart CI job)")
class WorkloadRBACTests(unittest.TestCase):
    def rules(self, enabled):
        rendered = subprocess.check_output(
            [
                "helm", "template", "herdr-hub", "charts/herdr-hub", "-n", "herdr",
                "-f", "examples/values.yaml", "--set",
                "workloads.enabled=" + str(enabled).lower(),
                "--show-only", "templates/rbac.yaml",
            ],
            cwd=ROOT, text=True,
        )
        return [
            (json.loads(resources), json.loads(verbs))
            for resources, verbs in re.findall(
                r"resources:\s*(\[[^\n]*\])\s*\n\s*verbs:\s*(\[[^\n]*\])", rendered
            )
        ]

    def test_exec_requires_get_and_create(self):
        rules = self.rules(True)
        self.assertIn((["pods/exec"], ["get", "create"]), rules)
        self.assertIn((["pods/log"], ["get"]), rules)
        self.assertNotIn((["pods/log"], ["get", "create"]), rules)

    def test_disabled_feature_has_no_exec_or_log_grant(self):
        for resources, _ in self.rules(False):
            self.assertNotIn("pods/exec", resources)
            self.assertNotIn("pods/log", resources)


if __name__ == "__main__":
    unittest.main()
