#!/usr/bin/env python3
"""Unit tests for pull-from-registry.py (Python 3.7+ compatible)."""

import base64
import hashlib
import importlib.util
import io
import json
import os
import tarfile
import tempfile
import threading
import unittest
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


MODULE_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "pull-from-registry.py")
SPEC = importlib.util.spec_from_file_location("pull_from_registry", MODULE_PATH)
PULL_MOD = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(PULL_MOD)


def build_layer_tar_gz(files_map, bad_member=None):
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tf:
        for name, data in files_map.items():
            ti = tarfile.TarInfo(name=name)
            ti.size = len(data)
            ti.mode = 0o755 if name == "gatehouse" else 0o644
            tf.addfile(ti, io.BytesIO(data))
        if bad_member is not None:
            ti = tarfile.TarInfo(name=bad_member)
            payload = b"evil"
            ti.size = len(payload)
            tf.addfile(ti, io.BytesIO(payload))
    return buf.getvalue()


class MockRegistryState:
    def __init__(self, blob_bytes, cdn_base_url, version="v0.1.2"):
        self.blob_bytes = blob_bytes
        self.blob_digest = "sha256:" + hashlib.sha256(blob_bytes).hexdigest()
        self.cdn_base_url = cdn_base_url
        self.version = version
        self.expected_user = "testuser"
        self.expected_pass = "testsecret"
        self.issued_token = "tok_test_123456"
        self.blob_downloads = 0
        self.cdn_auth_leaks = 0

        self.amd64_manifest = json.dumps({
            "schemaVersion": 2,
            "mediaType": "application/vnd.oci.image.manifest.v1+json",
            "config": {
                "mediaType": "application/vnd.oci.image.config.v1+json",
                "digest": "sha256:" + ("0" * 64),
                "size": 2,
            },
            "layers": [
                {
                    "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
                    "digest": self.blob_digest,
                    "size": len(blob_bytes),
                }
            ],
        }).encode("utf-8")
        self.amd64_digest = "sha256:" + hashlib.sha256(self.amd64_manifest).hexdigest()

        self.index_manifest = json.dumps({
            "schemaVersion": 2,
            "mediaType": "application/vnd.oci.image.index.v1+json",
            "manifests": [
                {
                    "mediaType": "application/vnd.oci.image.manifest.v1+json",
                    "digest": "sha256:" + ("a" * 64),
                    "size": 100,
                    "platform": {"os": "unknown", "architecture": "unknown"},
                    "annotations": {"vnd.docker.reference.type": "attestation-manifest"},
                },
                {
                    "mediaType": "application/vnd.oci.image.manifest.v1+json",
                    "digest": "sha256:" + ("b" * 64),
                    "size": 100,
                    "platform": {"os": "linux", "architecture": "arm64"},
                },
                {
                    "mediaType": "application/vnd.oci.image.manifest.v1+json",
                    "digest": self.amd64_digest,
                    "size": len(self.amd64_manifest),
                    "platform": {"os": "linux", "architecture": "amd64"},
                },
            ],
        }).encode("utf-8")


def make_handlers(state):
    class CdnHandler(BaseHTTPRequestHandler):
        def log_message(self, format, *args):
            return

        def do_GET(self):
            if "Authorization" in self.headers:
                state.cdn_auth_leaks += 1
                self.send_response(400)
                self.end_headers()
                self.wfile.write(b"Authorization header must not be sent on cross-origin redirect")
                return
            state.blob_downloads += 1
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(len(state.blob_bytes)))
            self.end_headers()
            self.wfile.write(state.blob_bytes)

    class RegistryHandler(BaseHTTPRequestHandler):
        registry_base = ""

        def log_message(self, format, *args):
            return

        def _require_bearer(self):
            auth = self.headers.get("Authorization", "")
            if auth == "Bearer " + state.issued_token:
                return True
            self.send_response(401)
            self.send_header(
                "Www-Authenticate",
                'Bearer realm="%s/auth",service="mock-registry",scope="repository:team/gatehouse:pull"'
                % self.registry_base,
            )
            self.end_headers()
            return False

        def do_GET(self):
            parsed = urllib.parse.urlparse(self.path)
            if parsed.path == "/auth":
                expected_basic = "Basic " + base64.b64encode(
                    ("%s:%s" % (state.expected_user, state.expected_pass)).encode("utf-8")
                ).decode("ascii")
                if self.headers.get("Authorization") != expected_basic:
                    self.send_response(401)
                    self.end_headers()
                    return
                body = json.dumps({"token": state.issued_token}).encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return

            if not self._require_bearer():
                return

            if parsed.path == "/v2/team/gatehouse/manifests/v0.1.2":
                self.send_response(200)
                self.send_header("Content-Type", "application/vnd.oci.image.index.v1+json")
                self.send_header(
                    "Docker-Content-Digest",
                    "sha256:" + hashlib.sha256(state.index_manifest).hexdigest(),
                )
                self.end_headers()
                self.wfile.write(state.index_manifest)
                return

            if parsed.path == "/v2/team/gatehouse/manifests/" + state.amd64_digest:
                self.send_response(200)
                self.send_header("Content-Type", "application/vnd.oci.image.manifest.v1+json")
                self.send_header("Docker-Content-Digest", state.amd64_digest)
                self.end_headers()
                self.wfile.write(state.amd64_manifest)
                return

            if parsed.path == "/v2/team/gatehouse/blobs/" + state.blob_digest:
                self.send_response(302)
                self.send_header("Location", state.cdn_base_url + "/presigned-layer?sig=ok")
                self.end_headers()
                return

            self.send_response(404)
            self.end_headers()

    return RegistryHandler, CdnHandler


class PullFromRegistryTest(unittest.TestCase):
    def _start_servers(self, blob_bytes):
        cdn_server = ThreadingHTTPServer(("127.0.0.1", 0), BaseHTTPRequestHandler)
        cdn_port = cdn_server.server_port
        cdn_url = "http://127.0.0.1:%d" % cdn_port

        state = MockRegistryState(blob_bytes=blob_bytes, cdn_base_url=cdn_url)
        reg_handler, cdn_handler = make_handlers(state)
        cdn_server.RequestHandlerClass = cdn_handler

        reg_server = ThreadingHTTPServer(("127.0.0.1", 0), reg_handler)
        reg_port = reg_server.server_port
        reg_handler.registry_base = "http://127.0.0.1:%d" % reg_port

        t1 = threading.Thread(target=cdn_server.serve_forever, daemon=True)
        t2 = threading.Thread(target=reg_server.serve_forever, daemon=True)
        t1.start()
        t2.start()
        return state, reg_server, cdn_server, "127.0.0.1:%d/team/gatehouse" % reg_port

    def test_pull_with_index_and_cross_origin_redirect(self):
        fake_bin = b"#!/bin/sh\necho gatehouse v0.1.2\n"
        bin_sha = hashlib.sha256(fake_bin).hexdigest()
        sums = ("%s  gatehouse\n" % bin_sha).encode("utf-8")
        meta = json.dumps({
            "version": "v0.1.2",
            "commit": "abcdef1234567",
            "short_sha": "abcdef1",
            "built_at": "2026-11-23T00:00:00Z",
            "arch": "linux/amd64",
            "binary_sha256": bin_sha,
        }).encode("utf-8")
        blob = build_layer_tar_gz({
            "gatehouse": fake_bin,
            "SHA256SUMS": sums,
            "RELEASE_META": meta,
        })

        state, reg_srv, cdn_srv, image_ref = self._start_servers(blob)
        try:
            with tempfile.TemporaryDirectory() as tmp:
                res = PULL_MOD.pull_artifact(
                    image=image_ref,
                    tag="v0.1.2",
                    username="testuser",
                    password="testsecret",
                    out_dir=tmp,
                    insecure_http=True,
                )
                self.assertEqual(res["version"], "v0.1.2")
                self.assertEqual(res["binary_sha256"], bin_sha)
                self.assertEqual(res["digest"], state.amd64_digest)
                self.assertEqual(state.cdn_auth_leaks, 0)
                self.assertEqual(state.blob_downloads, 1)
                self.assertTrue(os.access(res["binary_path"], os.X_OK))
        finally:
            reg_srv.shutdown()
            reg_srv.server_close()
            cdn_srv.shutdown()
            cdn_srv.server_close()

    def test_check_only_does_not_download_blobs(self):
        fake_bin = b"binary"
        bin_sha = hashlib.sha256(fake_bin).hexdigest()
        blob = build_layer_tar_gz({
            "gatehouse": fake_bin,
            "SHA256SUMS": ("%s  gatehouse\n" % bin_sha).encode("utf-8"),
        })
        state, reg_srv, cdn_srv, image_ref = self._start_servers(blob)
        try:
            res = PULL_MOD.pull_artifact(
                image=image_ref,
                tag="v0.1.2",
                username="testuser",
                password="testsecret",
                check_only=True,
                insecure_http=True,
            )
            self.assertEqual(res["digest"], state.amd64_digest)
            self.assertEqual(state.blob_downloads, 0)
        finally:
            reg_srv.shutdown()
            reg_srv.server_close()
            cdn_srv.shutdown()
            cdn_srv.server_close()

    def test_tampered_sha256sums_rejected(self):
        fake_bin = b"real-binary"
        blob = build_layer_tar_gz({
            "gatehouse": fake_bin,
            "SHA256SUMS": (("f" * 64) + "  gatehouse\n").encode("utf-8"),
        })
        _state, reg_srv, cdn_srv, image_ref = self._start_servers(blob)
        try:
            with tempfile.TemporaryDirectory() as tmp:
                with self.assertRaises(RuntimeError) as ctx:
                    PULL_MOD.pull_artifact(
                        image=image_ref,
                        tag="v0.1.2",
                        username="testuser",
                        password="testsecret",
                        out_dir=tmp,
                        insecure_http=True,
                    )
                self.assertIn("SHA256 mismatch", str(ctx.exception))
        finally:
            reg_srv.shutdown()
            reg_srv.server_close()
            cdn_srv.shutdown()
            cdn_srv.server_close()

    def test_path_traversal_member_rejected(self):
        fake_bin = b"bin"
        bin_sha = hashlib.sha256(fake_bin).hexdigest()
        blob = build_layer_tar_gz(
            {
                "gatehouse": fake_bin,
                "SHA256SUMS": ("%s  gatehouse\n" % bin_sha).encode("utf-8"),
            },
            bad_member="../evil",
        )
        _state, reg_srv, cdn_srv, image_ref = self._start_servers(blob)
        try:
            with tempfile.TemporaryDirectory() as tmp:
                with self.assertRaises(RuntimeError) as ctx:
                    PULL_MOD.pull_artifact(
                        image=image_ref,
                        tag="v0.1.2",
                        username="testuser",
                        password="testsecret",
                        out_dir=tmp,
                        insecure_http=True,
                    )
                self.assertIn("Path traversal", str(ctx.exception))
        finally:
            reg_srv.shutdown()
            reg_srv.server_close()
            cdn_srv.shutdown()
            cdn_srv.server_close()


if __name__ == "__main__":
    unittest.main()
