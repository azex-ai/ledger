#!/usr/bin/env python3
"""Build actual Go/npm consumers outside the checkout and optionally start them.

DATABASE_URL must identify an already isolated acceptance database. This runner
does not create/drop clusters, run tests against that database, or publish packages.
"""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import tempfile
import time
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--api-port", type=int, default=48110)
    parser.add_argument("--web-port", type=int, default=43110)
    parser.add_argument("--start", action="store_true")
    parser.add_argument("--env-file", type=Path, help="Reuse an existing private env JSON; never rotate a running database signing key")
    args = parser.parse_args()
    source = Path(__file__).resolve().parent
    checkout = source.parents[1]
    output = args.output or Path(tempfile.mkdtemp(prefix="ledger-admin-consumer-"))
    output = output.resolve()
    if output.is_relative_to(checkout):
        raise SystemExit("Output must be outside the ledger checkout")
    if output.exists() and any(output.iterdir()):
        raise SystemExit("Output directory must be empty; existing runtimes are never overwritten")
    output.mkdir(parents=True, exist_ok=True)
    logs = output / "logs"
    logs.mkdir()
    artifacts = output / "artifacts"
    artifacts.mkdir()
    reused_env = None
    if args.env_file:
        reused_env = json.loads(args.env_file.read_text())
        if not isinstance(reused_env, dict) or not all(isinstance(k, str) and isinstance(v, str) for k, v in reused_env.items()):
            raise SystemExit("Private env must contain string keys and values")
    database_url = (reused_env or os.environ).get("DATABASE_URL")
    if args.start and not database_url:
        raise SystemExit("DATABASE_URL is required with --start")
    sha = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=checkout, text=True).strip()
    dirty = subprocess.check_output(["git", "status", "--porcelain"], cwd=checkout, text=True)
    # Keep an exact resource receipt even when an install/build fails. Consumers
    # clean up only this directory, never a wildcard across unrelated runtimes.
    (output / "stage.json").write_text(json.dumps({"directory": str(output), "source_sha": sha,
                                                 "source_dirty": bool(dirty), "status": "building"}, indent=2) + "\n")
    print(f"Staging {output}", flush=True)

    def run(label, command, cwd, env=None, timeout=300):
        result = subprocess.run(command, cwd=cwd, env=env, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout)
        (logs / f"{label}.log").write_text(result.stdout)
        if result.returncode:
            raise SystemExit(f"{label} failed ({result.returncode}); see {logs / (label + '.log')}")
        print(f"PASS {label}", flush=True)
        return result.stdout

    run("sdk-build", ["npm", "run", "-w", "@azex/ledger-react", "build"], checkout / "web")
    packed = json.loads(run("npm-pack", ["npm", "pack", "--pack-destination", str(artifacts), "--json"],
                            checkout / "web/packages/ledger-react"))[0]
    tarball = artifacts / packed["filename"]
    backend = output / "backend"
    shutil.copytree(source / "backend", backend)
    (backend / "go.mod").write_text("module example.com/ledger-admin-host\n\ngo 1.26.6\n\n"
                                    "require github.com/azex-ai/ledger v0.0.0\n\n"
                                    f"replace github.com/azex-ai/ledger => {json.dumps(str(checkout))}\n")
    go_env = {**os.environ, "GOWORK": "off"}
    run("go-tidy", ["go", "mod", "tidy"], backend, go_env)
    run("go-build", ["go", "build", "-o", "ledger-admin-api", "."], backend, go_env)
    run("go-vet", ["go", "vet", "./..."], backend, go_env)
    deps = run("go-deps", ["go", "list", "-deps", "."], backend, go_env)
    if "testcontainers" in deps or "internal/postgrestest" in deps:
        raise SystemExit("Consumer production graph unexpectedly contains test fixtures")

    web = output / "web"
    shutil.copytree(source / "web", web, ignore=shutil.ignore_patterns("node_modules", ".next", "*.tsbuildinfo", ".env*"))
    install_env = {**os.environ, "NODE_ENV": "development"}
    run("npm-ci", ["npm", "ci", "--include=dev", "--ignore-scripts", "--no-audit", "--no-fund"], web, install_env)
    # Install the built archive, not a source alias/workspace link. This local
    # candidate override never writes an absolute path into the tracked lock.
    run("npm-candidate", ["npm", "install", "--include=dev", "--no-save", "--package-lock=false",
                           "--ignore-scripts", "--no-audit", "--no-fund", str(tarball)], web, install_env)
    if (web / "node_modules/@heroui/react").exists():
        raise SystemExit("HeroUI must be absent from this shadcn consumer")
    if (web / "node_modules/@azex/ledger-react").is_symlink():
        raise SystemExit("Ledger npm dependency must be a tarball installation")
    run("next-build", ["npm", "run", "build"], web, {**os.environ, "NODE_ENV": "production"})
    run("web-types", ["npm", "run", "typecheck"], web)
    run("web-auth-tests", ["npm", "test"], web)

    env = {"NODE_ENV": "production", "ENV": "production",
           "HTTP_PORT": str(args.api_port), "ADMIN_ORIGIN": f"http://127.0.0.1:{args.web_port}",
           "LEDGER_API_URL_INTERNAL": f"http://127.0.0.1:{args.api_port}",
           "CORS_ALLOWED_ORIGIN": f"http://127.0.0.1:{args.web_port}",
           "LEDGER_CACHE_BACKEND_ID": "admin-acceptance-" + secrets.token_hex(8),
           "ADMIN_SESSION_SECRET": secrets.token_hex(32),
           "LEDGER_SIGNING_SEED": base64.b64encode(secrets.token_bytes(32)).decode()}
    if database_url:
        env["DATABASE_URL"] = database_url
    keys = []
    for account, scope in [("reader", "read"), ("writer", "write"), ("admin", "admin"), ("reviewer", "read+deposit_review")]:
        key = secrets.token_urlsafe(32)
        env[f"LEDGER_KEY_{account.upper()}"] = key
        env[f"ADMIN_PASSWORD_{account.upper()}"] = secrets.token_urlsafe(18)
        keys.append(f"{account}:{scope}:{key}")
    env["API_KEYS"] = ",".join(keys)
    if reused_env is not None:
        env = reused_env
    if args.start and (env["HTTP_PORT"] != str(args.api_port) or
                       env["ADMIN_ORIGIN"] != f"http://127.0.0.1:{args.web_port}" or
                       env["LEDGER_API_URL_INTERNAL"] != f"http://127.0.0.1:{args.api_port}"):
        raise SystemExit("Private env ports/origin must match --api-port and --web-port")
    private = output / "private-env.json"
    fd = os.open(private, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as handle:
        json.dump(env, handle, indent=2)
    source_hash = hashlib.sha256()
    for path in sorted(source.rglob("*")):
        if not path.is_file() or any(part in ("node_modules", ".next", "__pycache__") for part in path.parts) or path.name.endswith(".tsbuildinfo") or (path.name.startswith(".env") and path.name != ".env.example"):
            continue
        source_hash.update(str(path.relative_to(source)).encode())
        source_hash.update(path.read_bytes())
    manifest = {"directory": str(output), "source_sha": sha, "source_dirty": bool(dirty), "source_status": dirty,
                "host_source_sha256": source_hash.hexdigest(), "package": packed["id"],
                "tarball_sha256": hashlib.sha256(tarball.read_bytes()).hexdigest(),
                "go_consumer": str(backend), "npm_consumer": str(web), "hero_installed": False,
                "private_env": str(private), "logs": str(logs), "origin": env["ADMIN_ORIGIN"], "pids": {}}
    processes = []
    try:
        if args.start:
            def start(name, command, cwd):
                with (logs / f"{name}.log").open("w") as log:
                    proc = subprocess.Popen(command, cwd=cwd, env={**os.environ, **env}, stdout=log,
                                            stderr=subprocess.STDOUT, start_new_session=True)
                processes.append(proc)
                manifest["pids"][name] = proc.pid
            start("api", [str(backend / "ledger-admin-api")], backend)
            start("next", ["node", "node_modules/next/dist/bin/next", "start", "--hostname", "127.0.0.1", "--port", str(args.web_port)], web)
            for url in [env["LEDGER_API_URL_INTERNAL"] + "/api/v1/system/ready", env["ADMIN_ORIGIN"] + "/login"]:
                deadline = time.monotonic() + 45
                while True:
                    if any(proc.poll() is not None for proc in processes):
                        raise RuntimeError(f"Runtime exited; see {logs}")
                    try:
                        with urllib.request.urlopen(url, timeout=2) as response:
                            if response.status == 200:
                                break
                    except OSError:
                        pass
                    if time.monotonic() >= deadline:
                        raise RuntimeError(f"Readiness timed out: {url}")
                    time.sleep(0.2)
        (output / "runtime.json").write_text(json.dumps(manifest, indent=2) + "\n")
        (output / "stage.json").write_text(json.dumps({"directory": str(output), "source_sha": sha,
                                                     "source_dirty": bool(dirty), "status": "ready"}, indent=2) + "\n")
        print(json.dumps({"runtime": str(output / "runtime.json"), "origin": env["ADMIN_ORIGIN"], "pids": manifest["pids"]}), flush=True)
    except BaseException:
        for proc in processes:
            proc.terminate()
        for proc in processes:
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)
        raise


if __name__ == "__main__":
    main()
