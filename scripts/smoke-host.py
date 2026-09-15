#!/usr/bin/env python3
"""Load the native plugin in a real CPA host, without real accounts or wake calls."""

import argparse
import json
from pathlib import Path
import secrets
import shutil
import socket
import subprocess
import time
import tempfile
import urllib.error
import urllib.request


def check(condition, message):
    if not condition:
        raise AssertionError(message)


def smoke(host, plugin, management_html=None, browser_test=False):
    with tempfile.TemporaryDirectory(prefix="codex-wakeup-smoke-") as directory:
        root = Path(directory)
        (root / "auth").mkdir()
        (root / "plugins").mkdir()
        shutil.copy2(plugin, root / "plugins/codex-wakeup.so")
        if management_html:
            (root / "static").mkdir()
            shutil.copy2(management_html, root / "static/management.html")
        key = secrets.token_urlsafe(32)
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        config = {
            "host": "127.0.0.1",
            "port": port,
            "auth-dir": str(root / "auth"),
            "remote-management": {
                "allow-remote": False,
                "secret-key": key,
                "disable-control-panel": not bool(management_html),
                "disable-auto-update-panel": True,
            },
            "plugins": {
                "enabled": True,
                "dir": str(root / "plugins"),
                "configs": {"codex-wakeup": {
                    "enabled": True,
                    "auto_wake": False,
                    "run_on_start": False,
                    "default_model": "smoke-model",
                    "state_file": "plugins/codex-wakeup/state.json",
                }},
            },
        }
        config_path = root / "config.yaml"
        # JSON is also YAML; no third-party Python packages are needed.
        config_path.write_text(json.dumps(config), encoding="utf-8")
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

        def request(path, method="GET", data=None, authenticated=True):
            headers = {"Authorization": "Bearer " + key} if authenticated else {}
            if data is not None:
                headers["Content-Type"] = "application/json"
            req = urllib.request.Request(
                f"http://127.0.0.1:{port}" + path,
                data=None if data is None else json.dumps(data).encode(),
                headers=headers, method=method,
            )
            with opener.open(req, timeout=5) as response:
                body = response.read()
                if "application/json" in response.headers.get("Content-Type", ""):
                    return json.loads(body)
                return body.decode("utf-8")

        def api(route, **kwargs):
            return request("/v0/management/codex-wakeup/" + route, **kwargs)

        log_path = root / "host.log"
        with log_path.open("w") as log:
            process = subprocess.Popen(
                [str(host), "-config", str(config_path), "-local-model"],
                cwd=root, stdout=log, stderr=subprocess.STDOUT,
            )
            try:
                deadline = time.monotonic() + 30
                while True:
                    check(process.poll() is None, "CPA exited before plugin became ready")
                    try:
                        overview = api("overview")
                        break
                    except urllib.error.URLError:
                        if time.monotonic() >= deadline:
                            raise RuntimeError("plugin routes did not become ready within 30s")
                        time.sleep(0.2)
                check(overview["plugin"] == "codex-wakeup", "plugin registration failed")
                check(overview["enabled"] and not overview["auto_wake"], "configuration was not forwarded")
                check(overview["default_model"] == "smoke-model", "config_yaml was not decoded")
                page = request("/v0/resource/plugins/codex-wakeup/status", authenticated=False)
                check("Codex 唤醒" in page, "native resource route is unavailable")
                embedded_page = api("ui")
                check("connect-src 'none'" in embedded_page, "embedded UI allows direct network access")
                try:
                    api("ui", authenticated=False)
                except urllib.error.HTTPError as error:
                    check(error.code == 401, "embedded document is not management-authenticated")
                else:
                    raise AssertionError("embedded document accepted an unauthenticated request")
                check(api("accounts")["accounts"] == [], "host.auth.list did not return an empty account list")

                task = {
                    "id": "smoke-task",
                    "name": "兼容 <test> & 'quotes'",
                    "prompt": 'Reply with <OK> & "done".',
                    "enabled": False,
                    "schedule": {"kind": "interval", "interval": "5h"},
                }
                # A single unauthorized mutation must be rejected by the host.
                try:
                    api("save-tasks", method="POST", data={"tasks": [task]}, authenticated=False)
                except urllib.error.HTTPError as error:
                    check(error.code == 401, f"unexpected authentication status: {error.code}")
                else:
                    raise AssertionError("host accepted an unauthenticated mutation")
                check(api("tasks")["tasks"] == [], "unauthenticated request changed state")

                saved = api("save-tasks", method="POST", data={"tasks": [task]})["tasks"][0]
                for returned in (saved, api("tasks")["tasks"][0]):
                    for field in ("name", "prompt"):
                        check(returned[field] == task[field], f"schema 6 text changed: {field}={returned[field]!r}")
                preview = api("preview", method="POST", data={"task": task})
                check(len(preview["preview"]) == 3, "schedule preview failed")
                # This ID has no auth file. Auto wake is off throughout the test.
                task["account_ids"] = ["smoke-missing-auth"]
                task["name"] = "更新 <test> & 'quotes'"
                updated = api("tasks", method="PUT", data={"task": task})["tasks"][0]
                check(updated["name"] == task["name"], "task update changed text")
                toggled = api("tasks", method="PATCH", data={"id": task["id"], "enabled": True})
                check(toggled["tasks"][0]["enabled"], "task toggle failed")
                persisted = json.loads((root / "plugins/codex-wakeup/state.json").read_text())
                check(persisted["tasks"][0]["name"] == task["name"], "task was not persisted")
                api("tasks", method="DELETE", data={"id": task["id"]})
                check(api("tasks")["tasks"] == [], "task deletion failed")
                task["id"] = "smoke-created"
                created = api("tasks", method="POST", data={"task": task})["tasks"][0]
                check(created["id"] == task["id"], "task creation failed")
                check(api("history")["history"] == [], "test unexpectedly executed a task")
                diagnostics = api("diagnostics")
                check(diagnostics["abi_version"] == 1, "unexpected ABI version")
                check(diagnostics["schema_version"] == 6, "schema 6 is not supported")
                check(diagnostics["scheduler_status"] == "auto_wake_disabled", "scheduler unexpectedly active")
                if browser_test:
                    check(management_html, "browser test requires --management-html")
                    subprocess.run(
                        ["node", str(Path(__file__).with_name("smoke-browser.cjs").resolve())],
                        input=json.dumps({"base": f"http://127.0.0.1:{port}", "key": key}),
                        text=True, check=True, timeout=120,
                    )
                print(f"CPA native smoke passed: codex-wakeup {overview['version']}; "
                      "registration, config, auth list, resource, management auth, "
                      "task CRUD/persistence/preview, raw JSON text, history and diagnostics")
            except Exception:
                print(log_path.read_text(errors="replace")[-12000:].replace(key, "[REDACTED]"))
                raise
            finally:
                process.terminate()
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", required=True, type=Path, help="CPA executable (with native plugin support)")
    parser.add_argument("--plugin", required=True, type=Path, help="Linux AMD64 codex-wakeup.so")
    parser.add_argument("--management-html", type=Path, help="Companion management.html")
    parser.add_argument("--browser-test", action="store_true", help="Run Playwright browser checks")
    args = parser.parse_args()
    smoke(args.host.resolve(strict=True), args.plugin.resolve(strict=True),
          args.management_html.resolve(strict=True) if args.management_html else None,
          args.browser_test)
