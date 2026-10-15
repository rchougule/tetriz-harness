#!/usr/bin/env python3
"""Check actual built image licence payloads; needs Docker, Python 3.9+ and Go.

Usage: scripts/image-licenses.py [--server IMAGE] [--python IMAGE]
Build first with docker build. Containers are temporary, never started for export.
This checks redistribution evidence, not complete source provenance/legal compliance.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parent.parent


def run(*args, **kwargs):
    return subprocess.check_output(args, text=True, **kwargs)


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def check_server(image):
    container = run("docker", "create", image).strip()
    try:
        with tempfile.TemporaryDirectory() as tmp:
            archive = Path(tmp) / "image.tar"
            subprocess.run(["docker", "export", "-o", str(archive), container], check=True)
            with tarfile.open(archive) as tar:
                def read(name):
                    entry = tar.extractfile(name)
                    require(entry is not None, f"Missing image file: {name}")
                    return entry.read()
                base = "usr/share/licenses/tetriz-harness/"
                files = [ROOT / "LICENSE", ROOT / "NOTICE"]
                files += [p for p in (ROOT / "third_party/licenses").rglob("*") if p.is_file()]
                for file in files:
                    rel = file.relative_to(ROOT).as_posix().replace("third_party/licenses/", "third_party/", 1)
                    require(read(base + rel) == file.read_bytes(), f"Image notice differs: {rel}")
                binary = Path(tmp) / "harnessctl-server"
                binary.write_bytes(read("usr/local/bin/harnessctl-server"))
                info = run("go", "version", "-m", str(binary), env={**os.environ, "GOTOOLCHAIN": "local"})
                rows = [line.split("\t") for line in read(base + "third_party/go/modules.tsv").decode().splitlines() if not line.startswith("#")]
                versions = {r[0]: r[1] for r in rows}
                require(info.splitlines()[0].endswith(versions["std"]), "Go toolchain differs from shipped inventory")
                for line in info.splitlines()[1:]:
                    parts = line.strip().split()
                    if parts and parts[0] == "dep":
                        require(versions.get(parts[1]) == parts[2], f"Linked Go module not attributed: {parts[1:3]}")
                require(any(n.startswith("usr/share/doc/") and n.endswith("/copyright") for n in tar.getnames()), "Distroless Debian copyright files missing")
                print(f"server: {len(files)} exact notice files; linked Go versions and base copyright payload verified")
    finally:
        subprocess.run(["docker", "rm", container], check=True, stdout=subprocess.DEVNULL)


# Execute inside the built image so metadata and native versions describe its wheels.
PYTHON_CHECK = r'''
import importlib.metadata as m
from pathlib import Path
import re, subprocess
root = Path('/usr/share/licenses/tetriz-py')
for f in ('LICENSE', 'NOTICE', 'THIRD_PARTY_LICENSES.md'):
    assert (root/f).is_file() and (root/f).stat().st_size, f
inventory = (root/'THIRD_PARTY_LICENSES.md').read_text().split('## Optional')[0]
normalize = lambda n: re.sub(r'[-_.]+', '-', n).lower()
count = 0
for d in m.distributions():
    name = normalize(d.metadata['Name'])
    if name == 'tetriz-py':
        continue
    assert re.search(r'\| '+re.escape(name)+r' \| '+re.escape(d.version)+r' \|', inventory), (name, d.version, 'inventory missing')
    files = [p for p in d.files or [] if re.search(r'(?:^|/)(?:licen[cs]e|copying)(?:[.-]|$)', str(p), re.I)]
    assert files, (name, 'no installed licence')
    for f in files:
        assert d.locate_file(f).is_file() and d.locate_file(f).stat().st_size, (name, str(f))
    for f in d.metadata.get_all('License-File', []):
        candidates = [p for p in d.files or [] if str(p).endswith('/'+f)]
        assert candidates and all(d.locate_file(p).is_file() and d.locate_file(p).stat().st_size for p in candidates), (name, 'declared licence missing', f)
    count += 1
from cryptography.hazmat.backends.openssl.backend import backend
import uvloop
assert backend.openssl_version_text().startswith('OpenSSL 4.0.3 '), 'review bundled OpenSSL notice'
assert uvloop.loop.libuv_get_version() == (1<<16 | 52<<8 | 1), 'review bundled libuv notice'
for f, marker in [('OpenSSL-LICENSE.txt', 'Apache License'), ('libuv-LICENSE', 'libuv project contributors')]:
    assert marker in (root/'native'/f).read_text(), ('missing native notice', f)
assert Path('/usr/local/lib/python3.11/LICENSE.txt').stat().st_size
packages = subprocess.check_output(['dpkg-query', '-W', '-f=${Package}\n'], text=True).splitlines()
for name in packages:
    assert (Path('/usr/share/doc')/name/'copyright').is_file(), ('Debian notice missing', name)
for d in m.distributions(path=['/usr/local/lib/python3.11/site-packages']):
    assert any('license' in str(p).lower() and d.locate_file(p).is_file() for p in d.files or []), ('global tool notice missing', d.metadata['Name'])
assert not any(normalize(d.metadata['Name']) in {'torch', 'sentence-transformers'} for d in m.distributions()), 'default image unexpectedly includes ML extra'
print(f'python: {count} runtime wheel inventories/licences; OpenSSL/libuv notices; {len(packages)} Debian copyright files; CPython/global tool licences verified')
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--server', default='tetriz-readiness-server:local')
    parser.add_argument('--python', default='tetriz-readiness-python:local')
    args = parser.parse_args()
    for image in (args.server, args.python):
        inspected = json.loads(run('docker', 'image', 'inspect', image))[0]
        print(f"{image}: {inspected['Id']} {inspected['Os']}/{inspected['Architecture']}", flush=True)
    check_server(args.server)
    subprocess.run(['docker', 'run', '--rm', '-i', '--entrypoint', 'python', args.python, '-'], input=PYTHON_CHECK, text=True, check=True)


if __name__ == '__main__':
    main()
