#!/usr/bin/env python3
"""Exercise an actual Terraform/OpenTofu CLI against a local fake VyOS API.

No live router or registry connection is required. Build the provider first.
Example: python3 scripts/test_cli.py --cli tofu
"""
import argparse
import copy
import http.server
import json
import os
from pathlib import Path
import platform
import shutil
import shlex
import subprocess
import tempfile
import threading
import urllib.parse


def ast(name, value):
    branch = isinstance(value, dict)
    return {
        "name": name,
        "data": {
            "values": [] if branch else value if isinstance(value, list) else [value],
            "comment": None, "tag": False, "leaf": not branch,
        },
        "children": [ast(k, v) for k, v in sorted(value.items())] if branch else [],
    }


class Router:
    def __init__(self):
        self.lock = threading.Lock()
        self.config = {"interfaces": {"dummy": {"dum99": {
            "description": "UNMANAGED", "address": ["192.0.2.99/32"],
        }}}}
        self.batches = []
        self.saves = 0

    def configure(self, operations):
        # Test fixture semantics: addresses are multi-value, remaining test
        # leaves are scalar. Production provider has no such schema knowledge.
        candidate = copy.deepcopy(self.config)
        for operation in operations:
            path = operation["path"]
            node = candidate
            if operation["op"] == "set":
                for key in path[:-2]:
                    node = node.setdefault(key, {})
                key, value = path[-2:]
                if key == "address":
                    values = node.setdefault(key, [])
                    if value not in values:
                        values.append(value)
                else:
                    node[key] = value
            else:
                for index, key in enumerate(path):
                    if isinstance(node, list):
                        node.remove(key)
                        break
                    if index == len(path) - 1:
                        node.pop(key)
                        break
                    node = node[key]
        self.config = candidate
        self.batches.append(operations)


def export_commands(path, value):
    if isinstance(value, dict):
        if not value and path:
            return "set " + shlex.join(path) + "\n"
        return "".join(export_commands(path + [key], item) for key, item in value.items())
    values = value if isinstance(value, list) else [value]
    return "".join("set " + shlex.join(path + [item]) + "\n" for item in values)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--cli", default="tofu")
    parser.add_argument("--provider-bin", default="bin/terraform-provider-vyoscmd_v0.1.0")
    args = parser.parse_args()
    cli = shutil.which(args.cli)
    if not cli:
        raise SystemExit(f"CLI not found: {args.cli}")
    binary = Path(args.provider_bin).resolve()
    if not binary.is_file():
        raise SystemExit("Build the provider with make build first")
    router = Router()

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            form = urllib.parse.parse_qs(self.rfile.read(int(self.headers["Content-Length"])).decode())
            assert form["key"] == ["fixture-api-key"]
            payload = json.loads(form["data"][0])
            with router.lock:
                if self.path == "/retrieve":
                    assert payload == {"op": "showConfig", "path": [], "configFormat": "json_ast"}
                    data = ast("", router.config)
                elif self.path == "/show":
                    assert payload == {"op": "show", "path": ["configuration", "commands"]}
                    data = export_commands([], router.config)
                elif self.path == "/configure":
                    assert isinstance(payload, list) and payload
                    router.configure(payload)
                    data = None
                elif self.path == "/config-file":
                    assert payload == {"op": "save"}
                    router.saves += 1
                    data = None
                else:
                    raise AssertionError(self.path)
            output = json.dumps({"success": True, "data": data, "error": None}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(output)))
            self.end_headers()
            self.wfile.write(output)

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with tempfile.TemporaryDirectory(prefix="vyoscmd-cli-") as folder:
            root = Path(folder)
            arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine(), platform.machine())
            target = f"{platform.system().lower()}_{arch}"
            for registry in ["registry.terraform.io", "registry.opentofu.org"]:
                mirror = root / "mirror" / registry / "houndsolo" / "vyoscmd" / "0.1.0" / target
                mirror.mkdir(parents=True)
                shutil.copy2(binary, mirror / "terraform-provider-vyoscmd_v0.1.0")
            (root / "cli.tfrc").write_text('provider_installation {\n filesystem_mirror { path = "' + str(root / "mirror") + '" }\n}\n')
            env = dict(os.environ, TF_CLI_CONFIG_FILE=str(root / "cli.tfrc"), TF_IN_AUTOMATION="1", TF_INPUT="0", VYOS_API_KEY="fixture-api-key", TF_LOG_PROVIDER="DEBUG", TF_LOG_PATH=str(root / "provider.log"), CHECKPOINT_DISABLE="1")

            def run(*command, code=0):
                result = subprocess.run([cli, *command], cwd=root, env=env, text=True, capture_output=True, timeout=90)
                if result.returncode != code:
                    raise AssertionError(f"{command}: exit {result.returncode}\n{result.stdout}\n{result.stderr}")
                return result.stdout

            def write(commands):
                config = {
                    "terraform": {"required_providers": {"vyoscmd": {"source": "houndsolo/vyoscmd", "version": "0.1.0"}}},
                    "provider": {"vyoscmd": {}},
                    "resource": {
                        "terraform_data": {"router": {"input": f"http://127.0.0.1:{server.server_port}"}},
                        "vyoscmd_commands": {"router": {"name": "leaf-11", "endpoint": "${terraform_data.router.output}", "commands": commands}},
                    },
                }
                (root / "main.tf.json").write_text(json.dumps(config))

            commands = ["set interfaces dummy dum99 mtu 1400", "set interfaces dummy dum99 address 192.0.2.1/32", "set interfaces dummy dum99 address 192.0.2.2/32", "delete protocols ospf"]
            write(commands)
            run("init", "-no-color")
            run("validate", "-no-color")
            run("plan", "-no-color", "-out=create.plan", "-detailed-exitcode", code=2)
            assert not router.batches
            assert "preview unavailable" in (root / "provider.log").read_text()
            run("apply", "-no-color", "create.plan")
            assert len(router.batches) == 1 and len(router.batches[0]) == 3
            run("plan", "-no-color", "-detailed-exitcode")
            before_id = json.loads(run("show", "-json"))["values"]["root_module"]["resources"][1]["values"]["id"]
            with router.lock:
                router.config["interfaces"]["dummy"]["dum99"]["mtu"] = "1420"
                router.config["protocols"] = {"ospf": {}}
            output = run("plan", "-no-color", "-out=drift.plan", "-detailed-exitcode", code=2)
            assert "in_sync" in output
            run("apply", "-no-color", "drift.plan")
            assert len(router.batches) == 2 and len(router.batches[-1]) == 2
            commands[0] = "set interfaces dummy dum99 mtu 1450"
            commands.remove("set interfaces dummy dum99 address 192.0.2.1/32")
            write(commands)
            run("plan", "-no-color", "-out=update.plan", "-detailed-exitcode", code=2)
            # Active state changes after saved plan: apply must recompute.
            with router.lock:
                router.config["interfaces"]["dummy"]["dum99"]["mtu"] = "1430"
            run("apply", "-no-color", "update.plan")
            assert len(router.batches) == 3
            assert router.batches[-1] == [
                {"op": "delete", "path": ["interfaces", "dummy", "dum99", "address", "192.0.2.1/32"]},
                {"op": "set", "path": ["interfaces", "dummy", "dum99", "mtu", "1450"]},
            ]
            run("plan", "-no-color", "-detailed-exitcode")
            resources = json.loads(run("show", "-json"))["values"]["root_module"]["resources"]
            assert resources[1]["values"]["id"] == before_id
            # Remove a complete policy subtree without leaving an invalid rule.
            write(commands + ["set policy as-path-list TEST rule 10 action permit", "set policy as-path-list TEST rule 10 regex '^$'"])
            run("apply", "-auto-approve", "-no-color")
            write(commands)
            run("apply", "-auto-approve", "-no-color")
            assert router.batches[-1] == [{"op": "delete", "path": ["policy"]}]
            with router.lock:
                router.config["protocols"]["ospf"] = {}
            run("destroy", "-auto-approve", "-no-color")
            assert router.config["interfaces"]["dummy"]["dum99"] == {"description": "UNMANAGED", "address": ["192.0.2.99/32"]}
            assert "ospf" in router.config["protocols"]
            assert len(router.batches) == 6 and router.saves == 6
            assert "fixture-api-key" not in (root / "provider.log").read_text()
            print(f"PASS: {run('version').splitlines()[0]}: create, no-op plan, SET/DELETE drift, saved-plan recomputation, pruning, destroy; six atomic batches")
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


if __name__ == "__main__":
    main()
