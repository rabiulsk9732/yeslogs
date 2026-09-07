#!/usr/bin/env python3
"""Build and atomically publish the static console. Never control natlog/systemd."""
import argparse
import contextlib
import fcntl
import hashlib
from html.parser import HTMLParser
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import tarfile
import tempfile
import urllib.error
import urllib.parse
import urllib.request

CONSOLE = "internal/director/web/console"
SHA = re.compile(r"^[0-9a-f]{40}$")


def git(repo, *args):
    env = dict(os.environ, GIT_TERMINAL_PROMPT="0")
    env.setdefault("GIT_SSH_COMMAND", "ssh -o BatchMode=yes -o ConnectTimeout=15")
    return subprocess.check_output(["git", "-C", str(repo), *args], env=env, timeout=90)


class Page(HTMLParser):
    def __init__(self):
        super().__init__()
        self.assets, self.scripts, self.ids = [], [], set()
        self.script = None

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        self.ids.add(attrs.get("id", ""))
        if tag == "script" and "src" not in attrs and attrs.get("type", "") in ("", "text/javascript", "module"):
            self.script = []
        if tag in ("script", "link", "img"):
            ref = attrs.get("src") or attrs.get("href")
            if ref:
                self.assets.append(ref)

    def handle_data(self, data):
        if self.script is not None:
            self.script.append(data)

    def handle_endtag(self, tag):
        if tag == "script" and self.script is not None:
            self.scripts.append("".join(self.script))
            self.script = None


def check_asset(root, base, ref):
    if ref.startswith("data:") or ref.startswith("#"):
        return
    if ref.startswith(("http:", "https:", "//")):
        raise ValueError(f"Console assets must be vendored: {ref}")
    path = urllib.parse.unquote(urllib.parse.urlsplit(ref).path)
    target = ((root / path.lstrip("/")) if path.startswith("/") else (base / path)).resolve()
    if not target.is_relative_to(root.resolve()) or not target.is_file():
        raise ValueError(f"Missing or escaping console asset: {ref}")
    return target


def validate(root):
    root = Path(root)
    page = Page()
    page.feed((root / "index.html").read_text())
    if not {"login", "li-email", "li-pass", "li-btn", "app"}.issubset(page.ids):
        raise ValueError("Console is missing required login/application elements")
    # Legacy vendor packages can include unused font families. Validate the
    # stylesheets actually loaded by the page, including their dependencies.
    pending = [check_asset(root, root, ref) for ref in page.assets]
    checked = set()
    while pending:
        css = pending.pop()
        if css is None or css.suffix != ".css" or css in checked:
            continue
        checked.add(css)
        for ref in re.findall(r"url\(\s*['\"]?([^)'\"]+)", css.read_text()):
            pending.append(check_asset(root, css.parent, ref.strip()))
    for source in page.scripts:
        subprocess.run(["node", "--check"], input=source, text=True, check=True, timeout=30)
    for script in root.rglob("*.js"):
        subprocess.run(["node", "--check", str(script)], check=True, timeout=30)


def unpack(data, destination):
    """Only ordinary files/directories; no links, devices or path traversal."""
    with tarfile.open(fileobj=io.BytesIO(data)) as archive:
        size = 0
        for member in archive:
            name = PurePosixPath(member.name)
            if name.is_absolute() or ".." in name.parts or not (member.isdir() or member.isfile()):
                raise ValueError(f"Unsafe console archive member: {member.name}")
            target = destination.joinpath(*name.parts)
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                size += member.size
                if size > 100 * 1024 * 1024:
                    raise ValueError("Console bundle exceeds 100 MiB")
                target.parent.mkdir(parents=True, exist_ok=True)
                with archive.extractfile(member) as src, target.open("wb") as dst:
                    shutil.copyfileobj(src, dst)
                target.chmod(0o644)


def build(repo, revision, output):
    revision = git(repo, "rev-parse", "--verify", revision + "^{commit}").decode().strip()
    if not SHA.fullmatch(revision):
        raise ValueError("Invalid commit ID")
    output = Path(output)
    if output.exists():
        raise ValueError(f"Refusing to overwrite an existing release: {output}")
    output.parent.mkdir(parents=True, exist_ok=True)
    staging = Path(tempfile.mkdtemp(prefix=".building-", dir=output.parent))
    try:
        unpack(git(repo, "archive", "--format=tar", f"{revision}:{CONSOLE}"), staging)
        validate(staging)
        index = staging / "index.html"
        index.write_text(index.read_text().replace("/assets/", f"/_ui/{revision}/assets/"))
        (staging / "ui-version.json").write_text(json.dumps({"revision": revision}) + "\n")
        hashes = {str(p.relative_to(staging)): hashlib.sha256(p.read_bytes()).hexdigest()
                  for p in sorted(staging.rglob("*")) if p.is_file()}
        (staging / "manifest.json").write_text(json.dumps(hashes, indent=2) + "\n")
        staging.chmod(0o755)
        staging.rename(output)
    except Exception:
        shutil.rmtree(staging, ignore_errors=True)
        raise
    return revision


def check_backend(repo, baseline, candidate, management_revision=None):
    changes = git(repo, "diff", "--name-only", baseline, candidate, "--",
                  "cmd", "internal", "configs", "go.mod", "go.sum",
                  ":(exclude)" + CONSOLE).decode().splitlines()
    if management_revision:
        if not SHA.fullmatch(management_revision):
            raise ValueError("Management revision must be a full commit ID")
        # The separately installed Director owns only control-plane code. Other
        # collector code and shared dependencies still match the original binary.
        managed = [p for p in changes if p.startswith(("internal/director/", "cmd/director/"))]
        if managed:
            drift = git(repo, "diff", "--name-only", management_revision, candidate,
                        "--", *managed).decode().splitlines()
            if not drift:
                changes = [p for p in changes if p not in managed]
    if changes:
        raise ValueError("UI release needs a separately deployed backend; blocked: " + ", ".join(changes[:12]))


def probe_management(config):
    revision = config.get("management_revision")
    if not revision:
        return
    url = config.get("management_probe_url", "")
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme != "http" or parsed.hostname != "127.0.0.1" or not parsed.port:
        raise ValueError("Management probe must use an explicit loopback HTTP port")
    with urllib.request.urlopen(url, timeout=10) as response:
        if json.load(response).get("revision") != revision:
            raise ValueError("Installed management backend revision does not match configuration")


def verify_release(release):
    hashes = json.loads((release / "manifest.json").read_text())
    for name, digest in hashes.items():
        path = release / name
        if not path.resolve().is_relative_to(release.resolve()) or path.is_symlink():
            raise ValueError("Release manifest contains an unsafe path")
        if hashlib.sha256(path.read_bytes()).hexdigest() != digest:
            raise ValueError(f"Release checksum mismatch: {name}")


def switch(root, target):
    link = root / ".current-next"
    link.unlink(missing_ok=True)
    link.symlink_to(target)
    os.replace(link, root / "current")


def probe(config, revision):
    for path in ("/ui-version.json", "/healthz", "/api/v1/me"):
        request = urllib.request.Request(config["probe_url"].rstrip("/") + path,
                                         headers={"Host": config["probe_host"], "Cache-Control": "no-cache"})
        try:
            with urllib.request.urlopen(request, timeout=10) as response:
                body = response.read()
                if path == "/ui-version.json" and json.loads(body)["revision"] != revision:
                    raise ValueError("Origin still serves a different UI revision")
                if path == "/healthz" and body.strip() != b"ok":
                    raise ValueError("Backend health check failed")
        except urllib.error.HTTPError as error:
            if path != "/api/v1/me" or error.code != 401:
                raise


def activate(root, revision, check):
    if not SHA.fullmatch(revision):
        raise ValueError("Activation requires a full commit ID")
    root = Path(root)
    release = root / "releases" / revision
    verify_release(release)
    old = os.readlink(root / "current") if (root / "current").is_symlink() else None
    switch(root, "releases/" + revision)
    try:
        check()
    except Exception:
        if old is not None:
            switch(root, old)
        else:
            (root / "current").unlink()
        raise
    if old is not None and old != "releases/" + revision:
        previous = root / "previous"
        previous.unlink(missing_ok=True)
        previous.symlink_to(old)


@contextlib.contextmanager
def deployment_lock(root):
    root = Path(root)
    root.mkdir(parents=True, exist_ok=True)
    with (root / ".deploy.lock").open("w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        yield


def fetch(config):
    repo, root = Path(config["repository"]), Path(config["root"])
    with deployment_lock(root):
        if not repo.exists():
            repo.parent.mkdir(parents=True, exist_ok=True)
            subprocess.run(["git", "init", "--bare", str(repo)], check=True)
            git(repo, "remote", "add", "origin", config["remote"])
        if git(repo, "remote", "get-url", "origin").decode().strip() != config["remote"]:
            raise ValueError("Fetcher repository has an unexpected origin")
        git(repo, "fetch", "--atomic", "--no-tags", "origin",
            "+refs/heads/main:refs/remotes/origin/main",
            "+refs/heads/console-live:refs/remotes/origin/console-live")
        revision = git(repo, "rev-parse", "refs/remotes/origin/console-live").decode().strip()
        # Only CI-promoted commits on main, and never silently move backwards.
        git(repo, "merge-base", "--is-ancestor", revision, "refs/remotes/origin/main")
        check_backend(repo, config["backend_revision"], revision, config.get("management_revision"))
        probe_management(config)
        current = root / "current" / "ui-version.json"
        if current.exists():
            old = json.loads(current.read_text())["revision"]
            if old == revision:
                print("Console already current: " + revision)
                return
            git(repo, "merge-base", "--is-ancestor", old, revision)
        release = root / "releases" / revision
        if not release.exists():
            build(repo, revision, release)
        activate(root, revision, lambda: probe(config, revision))
        print("Console published without restarting natlog: " + revision)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    builder = commands.add_parser("build")
    builder.add_argument("--repo", default=".")
    builder.add_argument("--revision", default="HEAD")
    builder.add_argument("--output", required=True)
    fetcher = commands.add_parser("fetch")
    fetcher.add_argument("--config", default="/etc/natlog/console-release.json")
    rollback = commands.add_parser("rollback")
    rollback.add_argument("revision")
    rollback.add_argument("--config", default="/etc/natlog/console-release.json")
    args = parser.parse_args()
    if args.command == "build":
        print(build(args.repo, args.revision, args.output))
    else:
        config = json.loads(Path(args.config).read_text())
        if args.command == "fetch":
            fetch(config)
        else:
            with deployment_lock(config["root"]):
                activate(config["root"], args.revision, lambda: probe(config, args.revision))
            print("Console rolled back to " + args.revision)


if __name__ == "__main__":
    main()
