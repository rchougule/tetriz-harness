#!/usr/bin/env python3
"""Negative tests against actual local Docker images (build both normal images first).

Overlays corrupt licence files in disposable derived images, checks rejection, then
removes only those test images. No package installation or model call is made.
"""
import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest
import uuid

spec = importlib.util.spec_from_file_location('image_licenses', Path(__file__).with_name('image-licenses.py'))
checks = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checks)


class ImageLicenseRegression(unittest.TestCase):
    def corrupt(self, base, destination, content):
        tag = 'tetriz-license-negative:' + uuid.uuid4().hex
        self.addCleanup(subprocess.run, ['docker', 'image', 'rm', tag], check=True, stdout=subprocess.DEVNULL)
        with tempfile.TemporaryDirectory() as directory:
            context = Path(directory)
            (context/'bad').write_text(content)
            (context/'Dockerfile').write_text(f'FROM {base}\nCOPY bad {destination}\n')
            subprocess.run(['docker', 'build', '-q', '-t', tag, directory], check=True, stdout=subprocess.DEVNULL)
        return tag

    def check_python_rejected(self, image, expected):
        result = subprocess.run(['docker', 'run', '--rm', '-i', '--entrypoint', 'python', image, '-'],
                                input=checks.PYTHON_CHECK, text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(expected, result.stderr)

    def test_corrupt_server_attribution_is_rejected(self):
        image = self.corrupt('tetriz-readiness-server:local', '/usr/share/licenses/tetriz-harness/NOTICE', 'removed')
        with self.assertRaisesRegex(RuntimeError, 'Image notice differs: NOTICE'):
            checks.check_server(image)

    def test_missing_bundled_native_notice_is_rejected(self):
        image = self.corrupt('tetriz-readiness-python:local', '/usr/share/licenses/tetriz-py/native/libuv-LICENSE', '')
        self.check_python_rejected(image, 'missing native notice')

    def test_missing_wheel_declared_notice_is_rejected(self):
        image = self.corrupt('tetriz-readiness-python:local',
                             '/app/.venv/lib/python3.11/site-packages/prometheus_client-0.26.0.dist-info/licenses/NOTICE', '')
        self.check_python_rejected(image, 'declared licence missing')


if __name__ == '__main__':
    unittest.main()
