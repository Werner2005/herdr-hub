#!/usr/bin/env python3
"""Validate tag, chart and default images agree before publishing a release."""
import pathlib
import re
import sys


def release_version(tag):
    if not re.fullmatch(r"v[0-9]+\.[0-9]+(?:\.[0-9]+)?", tag):
        raise ValueError("Expected vMAJOR.MINOR or vMAJOR.MINOR.PATCH")
    version = tag[1:] + (".0" if tag.count(".") == 1 else "")
    root = pathlib.Path(__file__).resolve().parents[2]
    chart = (root / "charts/herdr-hub/Chart.yaml").read_text()
    for field in ("version", "appVersion"):
        match = re.search(rf'^{field}: "?([^"\n]+)"?$', chart, re.MULTILINE)
        if not match or match[1] != version:
            raise ValueError(f"Chart {field} must be {version}")
    values = (root / "charts/herdr-hub/values.yaml").read_text()
    if f"workspace:{tag}\n" not in values or f"  tag: {tag}\n" not in values:
        raise ValueError("Update both default image tags to match the release")
    return version


if __name__ == "__main__":
    try:
        print(release_version(sys.argv[1]))
    except (IndexError, ValueError) as error:
        sys.exit(str(error))
