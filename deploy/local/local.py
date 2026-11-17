#!/usr/bin/env python3
"""Project-scoped k3d lifecycle. Requires Python 3.9+, no Python packages."""
import argparse
import contextlib
import fcntl
import json
import os
from pathlib import Path
import re
import shutil
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[2]
HERE = Path(__file__).resolve().parent
STATE = ROOT / ".local"
CLUSTER = "sandbox-cloud-local"
CONTEXT = "k3d-" + CLUSTER
# k3d preserves the explicit registries.create.name in k3d.yaml.
REGISTRY = "sandbox-cloud-registry.localhost"
PUSH = "127.0.0.1:5001"
PULL = REGISTRY + ":5000"


def run(*args, capture=False, **kwargs):
    print("+ " + " ".join(map(str, args)), flush=True)
    if str(args[0]) == "k3d":
        # k3d start/delete may read or update kubeconfig as a side effect too.
        kwargs["env"] = {**os.environ, "KUBECONFIG": str(STATE / "kubeconfig")}
    result = subprocess.run(list(map(str, args)), cwd=ROOT, text=True, check=True,
                            stdout=subprocess.PIPE if capture else None, **kwargs)
    return result.stdout.strip() if capture else None


def require(*names):
    missing = [name for name in names if shutil.which(name) is None]
    if missing:
        raise RuntimeError("Install prerequisites first: " + ", ".join(missing))


def kube(*args, **kwargs):
    return run("kubectl", "--kubeconfig", STATE / "kubeconfig", "--context", CONTEXT,
               *args, **kwargs)


def inventory():
    clusters = json.loads(run("k3d", "cluster", "list", "-o", "json", capture=True))
    registries = json.loads(run("k3d", "registry", "list", "-o", "json", capture=True))
    return (any(c["name"] == CLUSTER for c in (clusters or [])),
            any(r["name"] == REGISTRY for r in (registries or [])))


def ownership(create=False):
    engine = run("docker", "info", "--format", "{{.ID}}", capture=True)
    if not engine:
        raise RuntimeError("Docker engine has no ID; cannot establish ownership.")
    marker = STATE / "owner.json"
    if marker.exists():
        if json.loads(marker.read_text()) != {"cluster": CLUSTER, "engine": engine}:
            raise RuntimeError("Local state belongs to another Docker engine. Select the original context.")
        return
    if not create:
        raise RuntimeError("No local ownership record. Run make local-up first.")
    if any(inventory()):
        raise RuntimeError("Project cluster/registry already exists without an ownership record; refusing to adopt it.")
    marker.write_text(json.dumps({"cluster": CLUSTER, "engine": engine}) + "\n")


def image_digest(target, tag):
    image = f"{PUSH}/{target}:{tag}"
    run("docker", "build", "--target", target, "--tag", image, ".")
    run("docker", "push", image)
    digests = json.loads(run("docker", "image", "inspect", "--format",
                             "{{json .RepoDigests}}", image, capture=True))
    for value in digests or []:
        if value.startswith(f"{PUSH}/{target}@"):
            digest = value.split("@", 1)[1]
            if re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
                return f"{PULL}/{target}@{digest}"
    raise RuntimeError(f"Cannot resolve pushed digest for {image}")


def up():
    require("docker", "k3d", "kubectl")
    ownership(create=True)
    cluster, registry = inventory()
    if cluster:
        if not registry:
            raise RuntimeError("Owned cluster is missing its registry. Use local-down then local-up to recreate.")
        run("docker", "start", REGISTRY)
        run("k3d", "cluster", "start", CLUSTER)
    else:
        if registry:
            raise RuntimeError("Partial setup: owned registry exists without cluster. Run local-down then retry.")
        run("k3d", "cluster", "create", "--config", HERE / "k3d.yaml")
    config = run("k3d", "kubeconfig", "get", CLUSTER, capture=True)
    config_path = STATE / "kubeconfig"
    # kubeconfig contains credentials. Never print it or use the default file.
    fd = os.open(config_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as output:
        output.write(config + "\n")
    os.chmod(config_path, 0o600)
    tag = uuid.uuid4().hex
    control, workspace = image_digest("control", tag), image_digest("workspace", tag)
    rendered = run(sys.executable, ROOT / "deploy/render.py", "--control-image", control,
                   "--workspace-image", workspace, "--warm-pool-size", "0", capture=True)
    deployment = STATE / "api.yaml"
    deployment.write_text(rendered + "\n")
    # The shared foundation is outside this overlay directory. Allow that one
    # checked-in reference when rendering, then apply the rendered YAML.
    foundation = run("kubectl", "kustomize", HERE, "--load-restrictor",
                     "LoadRestrictionsNone", capture=True)
    kube("apply", "-f", "-", input=foundation)
    kube("apply", "--dry-run=server", "-f", deployment)
    kube("apply", "-f", deployment)
    kube("-n", "sandbox-control", "rollout", "status", "deployment/sandbox-api", "--timeout=180s")
    print("\nReady. Run make local-port-forward in another terminal.")


def port_forward(port=8080):
    return ["kubectl", "--kubeconfig", str(STATE / "kubeconfig"), "--context", CONTEXT,
            "-n", "sandbox-control", "port-forward", "--address=127.0.0.1",
            "service/sandbox-api", f"{port}:8080"]


def wait_until(check, timeout, description):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(0.5)
    raise RuntimeError("Timed out: " + description)


def expect_failed_command(sandbox, workspace, command, state, exit_code=None):
    try:
        sandbox("--json", "exec", "--timeout", "1s", workspace, "--", *command, capture=True)
    except subprocess.CalledProcessError as error:
        events = [json.loads(line) for line in (error.stdout or "").splitlines()]
        results = [event["result"] for event in events if event.get("result")]
        if not results or results[-1]["state"] != state:
            raise RuntimeError(f"Expected {state} result, not a transport or submission error") from error
        if exit_code is not None and (error.returncode != exit_code or results[-1]["exit_code"] != exit_code):
            raise RuntimeError(f"Expected remote and CLI exit code {exit_code}") from error
    else:
        raise RuntimeError("Failed command unexpectedly succeeded")


@contextlib.contextmanager
def connection():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    with (STATE / "port-forward.log").open("w") as log:
        process = subprocess.Popen(port_forward(port), cwd=ROOT, stdout=log, stderr=log)
        try:
            def ready():
                if process.poll() is not None:
                    raise RuntimeError("Port-forward exited; see .local/port-forward.log")
                try:
                    with urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=1) as response:
                        return response.status == 200
                except (OSError, urllib.error.URLError):
                    return False
            wait_until(ready, 30, "API port-forward")
            yield f"http://127.0.0.1:{port}"
        finally:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()


def check():
    require("docker", "k3d", "kubectl")
    ownership()
    cli = ROOT / "bin/sandbox"
    if not cli.is_file():
        raise RuntimeError("Build the CLI first: GOTOOLCHAIN=local go mod download && make build")
    with connection() as api:
        def sandbox(*args, capture=False):
            return run(cli, "--api", api, *args, capture=capture)

        def gone(workspace):
            items = json.loads(sandbox("--json", "list", capture=True))
            pods = json.loads(kube("-n", "sandbox-workspaces", "get", "pods", "-o", "json", capture=True))
            return not any(w["id"] == workspace for w in (items or [])) and not pods["items"]

        # Never take over or delete somebody's existing workspace.
        if json.loads(sandbox("--json", "list", capture=True)):
            raise RuntimeError("Destroy existing local workspaces before running local-check.")
        workspace = None
        try:
            workspace = json.loads(sandbox("--json", "create", "--ttl", "5m", capture=True))["id"]
            sandbox("put", workspace, ROOT / "examples/hello", ".")
            sandbox("exec", "--timeout", "180s", workspace, "--", "go", "test", "./...")
            expect_failed_command(sandbox, workspace, ["sh", "-c", "exit 7"], "failed", 7)
            expect_failed_command(sandbox, workspace, ["sleep", "30"], "timed_out")
            kube("-n", "sandbox-control", "rollout", "restart", "deployment/sandbox-api")
            kube("-n", "sandbox-control", "rollout", "status", "deployment/sandbox-api", "--timeout=120s")
            # Service port-forward is bound to the old Pod; reconnect after restart.
        finally:
            if workspace:
                # Open a new connection even when the old API Pod has disappeared.
                with connection() as cleanup_api:
                    try:
                        run(cli, "--api", cleanup_api, "inspect", workspace)
                    finally:
                        run(cli, "--api", cleanup_api, "destroy", workspace)
        with connection() as api_after:
            api = api_after
            wait_until(lambda: gone(workspace), 30, "workspace deletion")
            workspace = json.loads(sandbox("--json", "create", "--ttl", "1m", capture=True))["id"]
            try:
                print("Waiting for the one-minute TTL and physical cleanup...")
                wait_until(lambda: gone(workspace), 100, "TTL cleanup")
            finally:
                sandbox("destroy", workspace)
    print("Local smoke checks passed. See deploy/local/README.md for extended acceptance checks.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["up", "port-forward", "check", "stop", "down"])
    parser.add_argument("--confirm", action="store_true", help="allow deletion of the owned local cluster and registry")
    args = parser.parse_args()
    STATE.mkdir(mode=0o700, exist_ok=True)
    # Prevent two terminals from racing setup/cleanup.
    with (STATE / "lifecycle.lock").open("w") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            parser.error("Another local lifecycle command is running.")
        if args.action == "up":
            up()
        elif args.action == "check":
            check()
        else:
            require("docker", "k3d", *(["kubectl"] if args.action == "port-forward" else []))
            ownership()
            if args.action == "port-forward":
                # Release the lifecycle lock while the foreground connection runs.
                fcntl.flock(lock, fcntl.LOCK_UN)
                os.execvp("kubectl", port_forward())
            elif args.action == "stop":
                print("Stopping local runtime. Active commands will be interrupted.", flush=True)
                cluster, registry = inventory()
                if cluster:
                    run("k3d", "cluster", "stop", CLUSTER)
                if registry:
                    run("docker", "stop", REGISTRY)
            elif args.action == "down":
                if not args.confirm:
                    parser.error("Deletes local workspaces and registry images. Use make local-down CONFIRM=yes.")
                cluster, registry = inventory()
                if cluster:
                    run("k3d", "cluster", "delete", CLUSTER)
                if registry:
                    run("k3d", "registry", "delete", REGISTRY)
                for name in ["owner.json", "kubeconfig", "api.yaml", "port-forward.log"]:
                    (STATE / name).unlink(missing_ok=True)


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, subprocess.CalledProcessError, OSError, ValueError) as error:
        print(f"local setup: {error}", file=sys.stderr)
        sys.exit(1)
