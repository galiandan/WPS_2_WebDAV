"""Service child process for contract tests.

Builds the REAL adapter entrypoint (``wps_adapter.__main__.main``) with the
production environment variables, then patches only the WPS HTTP transport
to the in-process fake upstream. This keeps configuration parsing, secure
file handling, storage routing, and server lifecycle on the production path.

Usage (spawned by contract_tests/harness.py, not run by hand):

    python python_service.py --port N --scenario S.json \
        --record R.jsonl --stats T.json
"""

from __future__ import annotations

import argparse
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
PROJECT_ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.join(PROJECT_ROOT, "src"))

import fake_upstream  # noqa: E402


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--scenario", required=True)
    parser.add_argument("--record", required=True)
    parser.add_argument("--stats", required=True)
    args = parser.parse_args()

    with open(args.scenario, encoding="utf-8") as handle:
        scenario = json.load(handle)
    fake = fake_upstream.FakeUpstream(scenario, args.record, args.stats)

    import wps_adapter.__main__ as adapter_main
    import wps_adapter.storage as storage_mod

    settings_override = os.environ.get("CONTRACT_WEB_SETTINGS_FILE")
    if settings_override:
        # The production entrypoint hardcodes /etc/wps-adapter/secrets for
        # web settings; tests redirect that to a private temp directory.
        import wps_adapter.settings as settings_mod

        real_settings = settings_mod.WebSettings

        class PatchedWebSettings(real_settings):
            def __init__(self, *args, **kwargs):  # noqa: ANN002, ANN003
                kwargs.setdefault("file_path", settings_override)
                super().__init__(*args, **kwargs)

        settings_mod.WebSettings = PatchedWebSettings
        adapter_main.WebSettings = PatchedWebSettings

    if os.environ.get("CONTRACT_TRACEBACKS"):
        # Debug aid: print full tracebacks for unexpected request failures.
        import traceback as _tb

        import wps_adapter.server as _server_mod

        _orig_handle = _server_mod.AdapterRequestHandler._handle_exception

        def _traced_handle(self, exc, *, rest=False):  # noqa: ANN001
            _tb.print_exception(exc, file=sys.stderr)
            return _orig_handle(self, exc, rest=rest)

        _server_mod.AdapterRequestHandler._handle_exception = _traced_handle

    real_client = adapter_main.WpsDriveClient

    def patched_client(config, **_kwargs):  # noqa: ANN001
        return real_client(config, opener=fake, https_connection_factory=fake.signed_connection)

    # Both the single-space path (__main__) and the per-space child clients
    # (storage.MultiSpaceStorage) must use the faked transport.
    adapter_main.WpsDriveClient = patched_client
    storage_mod.WpsDriveClient = patched_client

    argv = [
        "serve",
        "--bind",
        os.environ.get("ADAPTER_BIND", "127.0.0.1"),
        "--port",
        str(args.port),
    ]
    return adapter_main.main(argv)


if __name__ == "__main__":
    raise SystemExit(main())
