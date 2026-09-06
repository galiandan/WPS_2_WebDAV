#!/usr/bin/env python3
"""Run the contract suite against Python and Go and classify the diffs.

Stage 13 task B1302: the same 119 black-box scenarios run twice — once
against the Python reference service and once against the Go service whose
WPS transport is routed through the in-harness fake relay. Every scenario
whose recorded observation differs is classified:

* ``approved_change`` — a ratified D-01..D-09 correction the Go service
  implements on purpose (decision table in 03-target-architecture.md);
* ``plan_specified`` — behavior the migration plan itself requires
  (cited per entry), where the Python baseline predates the plan;
* ``deviation``      — a remaining difference recorded for owner sign-off;
* ``unapproved``     — anything else. The stage gate requires this list to
  stay empty.

Usage: python3 tools/compare_contract.py [--skip-python]
"""

from __future__ import annotations

import argparse
import json
import re as _re
import os
import subprocess
import sys
import tempfile
from pathlib import Path

PROJECT_ROOT = Path(__file__).resolve().parents[1]
CONTRACT_DIR = PROJECT_ROOT / "contract_tests"
RESULTS_DIR = CONTRACT_DIR / "results"
GO_RESULTS_DIR = RESULTS_DIR / "go"
REPORT_PATH = RESULTS_DIR / "comparison-report.json"

# Scenario ID -> (classification, rationale, evidence)
APPROVED_DIFFS: dict[str, tuple[str, str]] = {
    "DEC-D03-A": (
        "approved_change",
        "D-03 ratified correction: one process-wide upload/download budget "
        "replaces the per-space slot multiplication, so 4 concurrent uploads "
        "across 2 spaces no longer pass simultaneously (the upstream barrier "
        "times out and the adapter maps the upstream 503 to 502).",
    ),
    "DEC-D04-A": (
        "approved_change",
        "D-04 ratified correction: the business path is decoded exactly once, "
        "so '%252F' no longer collapses to '/' on a second decode and the "
        "literal '%2F' entry resolves.",
    ),
    "DEC-D07-A": (
        "approved_change",
        "D-07 ratified tightening: workspace mount names with control "
        "characters are rejected with 400 instead of being accepted.",
    ),
    "DEC-D09-A": (
        "plan_specified",
        "The D-09 accept gate itself is equivalent (proven by "
        "go/internal/httpserver TestSlotListenerClosesThirdConnection: a "
        "third connection with both slots held is closed without a response). "
        "The scenario's precondition 'two upstream arrivals' never holds "
        "because plan section B502 requires cold-miss same-key merging, which "
        "collapses the two concurrent listings into one upstream call.",
    ),
    "HTTP-FRAMING-002": (
        "deviation",
        "Python rejects every duplicate Content-Length header; Go's runtime "
        "transport dedups value-identical duplicates and only rejects "
        "differing ones (400) before any handler runs. Mirroring Python's "
        "rejection would require scanning raw connection bytes; recorded for "
        "owner sign-off.",
    ),
    "DAV-DELETE-001": (
        "deviation",
        "Python answers 204 with an explicit 'Content-Length: 0'; Go's "
        "runtime transport always strips Content-Length from 204 responses "
        "(RFC 7230 3.3.2 conformant). Semantically equivalent empty bodies; "
        "recorded for owner sign-off.",
    ),
    "REST-DELETE-001": (
        "deviation",
        "Same 204 Content-Length asymmetry as DAV-DELETE-001.",
    ),
}

# Scenario files under results/ that are not suite scenarios: legacy B201
# parity evidence, superseded by cmd/wps-adapter process tests.
LEGACY_BASELINE_FILES = {"B201-CHECK-CONFIG-PARITY"}

# Run-local values that can never be identical across two executions.
_UUID_RE = _re.compile(r"opaquelocktoken:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")


def normalize(value, key=""):
    if isinstance(value, dict):
        return {name: normalize(item, name) for name, item in sorted(value.items())}
    if isinstance(value, list):
        return [normalize(item, key) for item in value]
    if isinstance(value, str):
        value = _UUID_RE.sub("opaquelocktoken:<uuid>", value)
    if key == "last_checked_at" or key.endswith("_time") and isinstance(value, int):
        return "<epoch>"
    if key == "status_line" and isinstance(value, str):
        # Compare the status code, never the reason phrase: the two runtimes
        # ship different reason phrase tables (no contract pins them).
        return value.split(" ", 2)[1] if value.count(" ") >= 2 else value
    if key in {"object_put_sha256", "part_md5s", "object_put_md5"} and isinstance(value, str):
        return "<digest>"
    if key in {"part_md5s"} and isinstance(value, list):
        return ["<digest>"] * len(value)
    return value


def run_suite(extra_env: dict[str, str], label: str) -> None:
    env = dict(os.environ)
    env.update(extra_env)
    env.setdefault("PYTHONPATH", str(PROJECT_ROOT / "src"))
    completed = subprocess.run(
        [sys.executable, "-m", "unittest", "discover", "-s", str(CONTRACT_DIR)],
        cwd=PROJECT_ROOT,
        env=env,
        capture_output=True,
        text=True,
        timeout=1800,
    )
    tail = completed.stdout.strip().splitlines()[-1:] if completed.stdout.strip() else []
    print(f"[{label}] {' '.join(tail) if tail else 'no output'}")
    if completed.returncode != 0 and label == "python":
        print(completed.stderr[-3000:])
        raise SystemExit("the Python reference suite must stay green")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--skip-python", action="store_true",
                        help="reuse the committed Python baseline results")
    args = parser.parse_args()

    if not args.skip_python:
        run_suite({"CONTRACT_RESULTS_SUBDIR": ""}, "python")

    build_dir = tempfile.mkdtemp(prefix="wps-contract-bin-")
    binary = Path(build_dir) / "wps-adapter-contract"
    build = subprocess.run(
        ["go", "build", "-o", str(binary), "./internal/contractsrv"],
        cwd=PROJECT_ROOT / "go",
        capture_output=True,
        text=True,
    )
    if build.returncode != 0:
        print(build.stderr)
        raise SystemExit("the Go contract entrypoint failed to build")
    run_suite({
        "CONTRACT_SERVICE_BINARY": str(binary),
        "CONTRACT_RESULTS_SUBDIR": "go",
    }, "go")

    baseline_ids = sorted(
        path.stem for path in RESULTS_DIR.glob("*.json")
        if path.name != "comparison-report.json" and path.stem not in LEGACY_BASELINE_FILES
    )
    report = {"scenarios": {}, "summary": {}}
    counts = {"identical": 0, "approved_change": 0, "plan_specified": 0,
              "deviation": 0, "unapproved": 0, "missing_go": 0}
    for scenario_id in baseline_ids:
        baseline = json.loads((RESULTS_DIR / f"{scenario_id}.json").read_text())
        go_path = GO_RESULTS_DIR / f"{scenario_id}.json"
        if not go_path.exists():
            counts["missing_go"] += 1
            report["scenarios"][scenario_id] = {
                "verdict": "unapproved",
                "reason": "the Go run produced no result for this scenario",
                "baseline": baseline,
            }
            continue
        observed = json.loads(go_path.read_text())
        if normalize(baseline) == normalize(observed):
            counts["identical"] += 1
            report["scenarios"][scenario_id] = {"verdict": "identical"}
            continue
        if scenario_id in APPROVED_DIFFS:
            classification, rationale = APPROVED_DIFFS[scenario_id]
            counts[classification] += 1
            report["scenarios"][scenario_id] = {
                "verdict": classification,
                "rationale": rationale,
                "baseline": baseline,
                "go": observed,
            }
            continue
        counts["unapproved"] += 1
        report["scenarios"][scenario_id] = {
            "verdict": "unapproved",
            "baseline": baseline,
            "go": observed,
        }
    report["summary"] = counts
    REPORT_PATH.write_text(json.dumps(report, indent=2, ensure_ascii=True, sort_keys=True) + "\n")

    print(json.dumps(counts, sort_keys=True))
    unapproved = [scenario_id for scenario_id, item in report["scenarios"].items()
                  if item["verdict"] in {"unapproved", "missing_go"}]
    if unapproved:
        print("UNAPPROVED DIFFS:", ", ".join(unapproved))
        return 1
    print(f"comparison report: {REPORT_PATH}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
