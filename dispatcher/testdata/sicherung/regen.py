"""Regenerates the *.golden.jsonl files from the Python reference (infra-l1 host/sicherung).

Usage (from this directory):
    uv run --no-project --with pydantic python regen.py <path to infra-l1/host/sicherung>

cases.json is the single source for the Go test and for this script; the goldens are the
interface to the Python side, so change them only by re-running this against the reference.
"""

import json
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(Path(sys.argv[1]).resolve()))

from erkennung import DiffStat, Schwellen, bewerte  # noqa: E402

for case in json.loads((HERE / "cases.json").read_text(encoding="utf-8")):
    diff = DiffStat(**case["diffstat"]) if case["diffstat"] else None
    payload = (HERE / case["fixture"]).read_text(encoding="utf-8")
    zeilen = "".join(b.model_dump_json() + "\n" for b in bewerte(case["event"], payload, Schwellen(), diff))
    (HERE / f"{case['name']}.golden.jsonl").write_text(zeilen, encoding="utf-8")
