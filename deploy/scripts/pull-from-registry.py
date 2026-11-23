#!/usr/bin/env python3
"""Pure-Python 3.7+ OCI / Docker Registry v2 artifact puller.

Designed for air-gapped-DNS or restricted Linux hosts (such as gpu1 on Kylin V10):
- Uses only Python standard library modules (compatible with Python 3.7.9+).
- Built-in UDP fallback DNS resolver (default: 223.5.5.5, 119.29.29.29) when system
  DNS (/etc/resolv.conf) is empty or fails, without modifying OS files.
- Strips Authorization headers across cross-origin 302/307 redirects so Aliyun OSS
  and Tencent COS pre-signed blob URLs work seamlessly.
- Verifies layer blob digests, guards against tar path traversal, and validates the
  extracted gatehouse binary against SHA256SUMS and RELEASE_META.
"""

import argparse
import base64
import hashlib
import json
import os
import posixpath
import re
import shutil
import socket
import struct
import sys
import tarfile
import tempfile
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Dict, List, Optional, Tuple


ORIG_GETADDRINFO = socket.getaddrinfo
DEFAULT_DNS_SERVERS = ("223.5.5.5", "119.29.29.29")
_DNS_CACHE = {}  # type: Dict[str, List[str]]

ACCEPT_MANIFESTS = ", ".join([
    "application/vnd.docker.distribution.manifest.v2+json",
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
    "application/vnd.oci.image.index.v1+json",
])


def _read_dns_name(data: bytes, offset: int) -> Tuple[str, int]:
    labels = []  # type: List[str]
    jumped = False
    orig_next = offset
    for _ in range(64):
        if offset >= len(data):
            raise OSError("Truncated DNS packet while reading name")
        length = data[offset]
        if length == 0:
            offset += 1
            break
        if (length & 0xC0) == 0xC0:
            if offset + 1 >= len(data):
                raise OSError("Truncated DNS pointer")
            ptr = ((length & 0x3F) << 8) | data[offset + 1]
            if not jumped:
                orig_next = offset + 2
            offset = ptr
            jumped = True
        else:
            offset += 1
            if offset + length > len(data):
                raise OSError("Truncated DNS label")
            labels.append(data[offset:offset + length].decode("ascii", "replace"))
            offset += length
    return ".".join(labels), (orig_next if jumped else offset)


def dns_resolve_a(
    name: str,
    servers: Tuple[str, ...] = DEFAULT_DNS_SERVERS,
    depth: int = 0,
    timeout: float = 3.0,
) -> List[str]:
    if depth > 8:
        raise OSError("DNS CNAME recursion limit exceeded for %s" % name)

    key = name.rstrip(".").lower()
    if key in _DNS_CACHE:
        return _DNS_CACHE[key]

    try:
        res = ORIG_GETADDRINFO(name, None, socket.AF_INET)
        if res:
            ips = []  # type: List[str]
            for item in res:
                ip = item[4][0]
                if ip not in ips:
                    ips.append(ip)
            if ips:
                _DNS_CACHE[key] = ips
                return ips
    except OSError:
        pass

    query = struct.pack("!HHHHHH", 0x4242, 0x0100, 1, 0, 0, 0)
    for part in key.split("."):
        encoded = part.encode("ascii")
        if not encoded or len(encoded) > 63:
            raise OSError("Invalid DNS label in %s" % name)
        query += bytes([len(encoded)]) + encoded
    query += b"\x00" + struct.pack("!HH", 1, 1)

    last_err = None  # type: Optional[Exception]
    for srv in servers:
        sock = None
        try:
            sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
            sock.settimeout(timeout)
            sock.sendto(query, (srv, 53))
            data, _ = sock.recvfrom(4096)
            if len(data) < 12:
                raise OSError("Short DNS response from %s" % srv)
            _, _, qdcount, ancount, _, _ = struct.unpack("!HHHHHH", data[:12])
            off = 12
            for _ in range(qdcount):
                _, off = _read_dns_name(data, off)
                off += 4
            ips = []
            cnames = []
            for _ in range(ancount):
                _, off = _read_dns_name(data, off)
                if off + 10 > len(data):
                    break
                rtype, _rclass, _ttl, rdlen = struct.unpack("!HHIH", data[off:off + 10])
                off += 10
                if off + rdlen > len(data):
                    break
                if rtype == 1 and rdlen == 4:
                    ips.append(".".join(str(b) for b in data[off:off + 4]))
                elif rtype == 5:
                    cname, _ = _read_dns_name(data, off)
                    cnames.append(cname)
                off += rdlen
            if ips:
                _DNS_CACHE[key] = ips
                return ips
            if cnames:
                ips = dns_resolve_a(cnames[0], servers=servers, depth=depth + 1, timeout=timeout)
                _DNS_CACHE[key] = ips
                return ips
        except Exception as exc:
            last_err = exc
        finally:
            if sock is not None:
                try:
                    sock.close()
                except Exception:
                    pass

    raise OSError("DNS lookup failed for %s via %s: %s" % (name, servers, last_err))


def install_fallback_dns(servers: Tuple[str, ...] = DEFAULT_DNS_SERVERS) -> None:
    if not servers:
        return

    def patched_getaddrinfo(host, port, family=0, type=0, proto=0, flags=0):
        try:
            return ORIG_GETADDRINFO(host, port, family, type, proto, flags)
        except OSError:
            if host is None:
                raise
            host_str = host.decode("ascii") if isinstance(host, bytes) else str(host)
            ips = dns_resolve_a(host_str, servers=servers)
            return ORIG_GETADDRINFO(ips[0], port, family, type, proto, flags)

    socket.getaddrinfo = patched_getaddrinfo


class StripAuthRedirectHandler(urllib.request.HTTPRedirectHandler):
    """Strip Authorization headers when redirected to a different origin (OSS/COS)."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        new_req = super().redirect_request(req, fp, code, msg, headers, newurl)
        if new_req is None:
            return None
        old_parsed = urllib.parse.urlparse(req.full_url)
        new_parsed = urllib.parse.urlparse(new_req.full_url)
        old_origin = (old_parsed.scheme.lower(), old_parsed.netloc.lower())
        new_origin = (new_parsed.scheme.lower(), new_parsed.netloc.lower())
        if old_origin != new_origin:
            for header_map in (new_req.headers, new_req.unredirected_hdrs):
                to_delete = [k for k in header_map if k.lower() == "authorization"]
                for k in to_delete:
                    del header_map[k]
        return new_req


def parse_image_ref(image: str, default_tag: str = "latest") -> Tuple[str, str, str]:
    image = image.strip()
    if "://" in image:
        image = image.split("://", 1)[1]
    if "/" not in image:
        raise ValueError("Image reference must include registry host and repository: %r" % image)
    registry, repo = image.split("/", 1)
    tag = default_tag
    if "@" in repo:
        repo, tag = repo.split("@", 1)
    elif ":" in repo.rsplit("/", 1)[-1]:
        repo, embedded_tag = repo.rsplit(":", 1)
        if default_tag == "latest":
            tag = embedded_tag
    repo = repo.strip("/")
    if not registry or not repo:
        raise ValueError("Invalid image reference: %r" % image)
    return registry, repo, tag


def parse_www_authenticate(header_value: str) -> Tuple[str, Dict[str, str]]:
    header_value = header_value.strip()
    parts = header_value.split(None, 1)
    if not parts:
        return "", {}
    scheme = parts[0].lower()
    params = {}  # type: Dict[str, str]
    if len(parts) > 1:
        for key, val in re.findall(r'([A-Za-z0-9_]+)="([^"]*)"', parts[1]):
            params[key.lower()] = val
    return scheme, params


class RegistryClient:
    def __init__(
        self,
        registry: str,
        repo: str,
        username: str = "",
        password: str = "",
        timeout: int = 60,
        insecure_http: bool = False,
    ) -> None:
        self.registry = registry
        self.repo = repo
        self.username = username
        self.password = password
        self.timeout = timeout
        self.scheme = "http" if insecure_http else "https"
        self.opener = urllib.request.build_opener(StripAuthRedirectHandler())
        self._tokens = {}  # type: Dict[str, str]

    def _basic_header(self) -> Optional[str]:
        if not self.username and not self.password:
            return None
        raw = ("%s:%s" % (self.username, self.password)).encode("utf-8")
        return "Basic " + base64.b64encode(raw).decode("ascii")

    def _fetch_bearer_token(self, params: Dict[str, str], default_scope: str) -> str:
        realm = params.get("realm")
        if not realm:
            raise RuntimeError("Bearer challenge missing realm: %r" % params)
        query = {}  # type: Dict[str, str]
        if params.get("service"):
            query["service"] = params["service"]
        scope = params.get("scope") or default_scope
        if scope:
            query["scope"] = scope

        parsed = urllib.parse.urlparse(realm)
        existing_qs = urllib.parse.parse_qsl(parsed.query)
        merged = dict(existing_qs)
        merged.update(query)
        token_url = urllib.parse.urlunparse((
            parsed.scheme,
            parsed.netloc,
            parsed.path,
            parsed.params,
            urllib.parse.urlencode(merged),
            parsed.fragment,
        ))
        headers = {"Accept": "application/json", "User-Agent": "precinct-gatehouse-cd/1.0"}
        basic = self._basic_header()
        if basic:
            headers["Authorization"] = basic
        req = urllib.request.Request(token_url, headers=headers)
        with self.opener.open(req, timeout=self.timeout) as resp:
            payload = json.loads(resp.read().decode("utf-8"))
        token = payload.get("token") or payload.get("access_token")
        if not token:
            raise RuntimeError("Token endpoint did not return token/access_token")
        return token

    def request(
        self,
        path: str,
        headers: Optional[Dict[str, str]] = None,
        scope: Optional[str] = None,
    ):
        url = "%s://%s%s" % (self.scheme, self.registry, path)
        default_scope = scope or ("repository:%s:pull" % self.repo)
        req_headers = {"User-Agent": "precinct-gatehouse-cd/1.0"}
        if headers:
            req_headers.update(headers)
        if default_scope in self._tokens and "Authorization" not in req_headers:
            req_headers["Authorization"] = self._tokens[default_scope]

        req = urllib.request.Request(url, headers=req_headers)
        try:
            return self.opener.open(req, timeout=self.timeout)
        except urllib.error.HTTPError as err:
            try:
                if err.code != 401:
                    raise
                challenge = err.headers.get("Www-Authenticate") or err.headers.get("www-authenticate") or ""
            finally:
                err.close()
            scheme, params = parse_www_authenticate(challenge)
            if scheme == "bearer":
                token = self._fetch_bearer_token(params, default_scope)
                auth_val = "Bearer " + token
                self._tokens[default_scope] = auth_val
            elif scheme == "basic":
                basic = self._basic_header()
                if not basic:
                    raise RuntimeError("Registry requires Basic authentication credentials")
                auth_val = basic
                self._tokens[default_scope] = auth_val
            else:
                raise RuntimeError("Unsupported 401 Www-Authenticate challenge: %r" % challenge)

            retry_headers = dict(req_headers)
            retry_headers["Authorization"] = auth_val
            retry_req = urllib.request.Request(url, headers=retry_headers)
            return self.opener.open(retry_req, timeout=self.timeout)

    def fetch_manifest(self, reference: str) -> Tuple[Dict[str, Any], str]:
        path = "/v2/%s/manifests/%s" % (self.repo, urllib.parse.quote(reference, safe=":@"))
        with self.request(path, headers={"Accept": ACCEPT_MANIFESTS}) as resp:
            raw = resp.read()
            digest = resp.headers.get("Docker-Content-Digest") or ""
        if not digest:
            digest = "sha256:" + hashlib.sha256(raw).hexdigest()
        doc = json.loads(raw.decode("utf-8"))
        return doc, digest

    def resolve_linux_amd64_manifest(self, tag: str) -> Tuple[Dict[str, Any], str]:
        doc, digest = self.fetch_manifest(tag)
        if "manifests" not in doc:
            return doc, digest

        selected = None  # type: Optional[Dict[str, Any]]
        for item in doc.get("manifests", []):
            annotations = item.get("annotations") or {}
            if annotations.get("vnd.docker.reference.type") == "attestation-manifest":
                continue
            plat = item.get("platform") or {}
            if plat.get("os") == "linux" and plat.get("architecture") == "amd64":
                selected = item
                break
        if selected is None or not selected.get("digest"):
            raise RuntimeError("No linux/amd64 manifest found in index for tag %r" % tag)
        return self.fetch_manifest(selected["digest"])

    def download_blob(self, digest: str, dest_path: str) -> None:
        if not digest.startswith("sha256:"):
            raise ValueError("Unsupported blob digest algorithm: %s" % digest)
        expected_hex = digest.split(":", 1)[1].lower()
        path = "/v2/%s/blobs/%s" % (self.repo, digest)
        hasher = hashlib.sha256()
        with self.request(path) as resp:
            with open(dest_path, "wb") as out_fp:
                while True:
                    chunk = resp.read(65536)
                    if not chunk:
                        break
                    hasher.update(chunk)
                    out_fp.write(chunk)
        actual_hex = hasher.hexdigest().lower()
        if actual_hex != expected_hex:
            raise RuntimeError(
                "Blob digest mismatch for %s: expected %s, got %s" % (digest, expected_hex, actual_hex)
            )


def _normalize_tar_member_name(raw_name: str) -> str:
    if "\x00" in raw_name:
        raise RuntimeError("Unsafe null byte in tar member name: %r" % raw_name)
    parts = [p for p in raw_name.replace("\\", "/").split("/") if p not in ("", ".")]
    if ".." in parts:
        raise RuntimeError("Path traversal detected in tar member: %r" % raw_name)
    if raw_name.startswith("../") or "/../" in raw_name or raw_name == "..":
        raise RuntimeError("Path traversal detected in tar member: %r" % raw_name)
    return "/".join(parts)


def extract_artifact_from_layer(blob_path: str, out_dir: str) -> Dict[str, str]:
    wanted = {"gatehouse", "SHA256SUMS", "RELEASE_META"}
    extracted = {}  # type: Dict[str, str]
    with tarfile.open(blob_path, "r:*") as tf:
        for member in tf.getmembers():
            if member.issym() or member.islnk() or member.ischr() or member.isblk() or member.isfifo():
                raise RuntimeError("Unsafe special/link tar member rejected: %r" % member.name)
            rel = _normalize_tar_member_name(member.name)
            if not rel:
                continue
            if posixpath.basename(rel) != rel and rel not in wanted:
                # Validate subpaths cleanly; reject traversal already checked above
                pass
            if rel in wanted and member.isfile():
                src = tf.extractfile(member)
                if src is None:
                    raise RuntimeError("Failed to read tar member: %r" % member.name)
                target_path = os.path.join(out_dir, rel)
                tmp_target = target_path + ".tmp"
                with open(tmp_target, "wb") as dst:
                    shutil.copyfileobj(src, dst)
                src.close()
                os.chmod(tmp_target, 0o755 if rel == "gatehouse" else 0o644)
                os.replace(tmp_target, target_path)
                extracted[rel] = target_path
    return extracted


def sha256_file(path: str) -> str:
    hasher = hashlib.sha256()
    with open(path, "rb") as fp:
        while True:
            chunk = fp.read(65536)
            if not chunk:
                break
            hasher.update(chunk)
    return hasher.hexdigest().lower()


def parse_sha256sums(path: str) -> Dict[str, str]:
    sums = {}  # type: Dict[str, str]
    with open(path, "r", encoding="utf-8") as fp:
        for line in fp:
            line = line.strip()
            if not line:
                continue
            parts = line.split()
            if len(parts) >= 2:
                digest = parts[0].lower()
                fname = posixpath.basename(parts[1].lstrip("*"))
                sums[fname] = digest
    return sums


def pull_artifact(
    image: str,
    tag: str,
    username: str,
    password: str,
    out_dir: Optional[str] = None,
    check_only: bool = False,
    timeout: int = 60,
    insecure_http: bool = False,
) -> Dict[str, Any]:
    registry, repo, resolved_tag = parse_image_ref(image, tag)
    client = RegistryClient(
        registry=registry,
        repo=repo,
        username=username,
        password=password,
        timeout=timeout,
        insecure_http=insecure_http,
    )
    manifest, digest = client.resolve_linux_amd64_manifest(resolved_tag)
    if check_only:
        return {
            "image": "%s/%s" % (registry, repo),
            "tag": resolved_tag,
            "digest": digest,
            "layer_count": len(manifest.get("layers", [])),
        }

    if not out_dir:
        raise ValueError("--out-dir is required unless --check-only is specified")
    os.makedirs(out_dir, exist_ok=True)

    layers = manifest.get("layers") or []
    if not layers:
        raise RuntimeError("Manifest %s contains no layers" % digest)

    found = {}  # type: Dict[str, str]
    for idx, layer in enumerate(layers):
        layer_digest = layer.get("digest") or ""
        fd, tmp_blob = tempfile.mkstemp(prefix=".blob-%d-" % idx, dir=out_dir)
        os.close(fd)
        try:
            client.download_blob(layer_digest, tmp_blob)
            found.update(extract_artifact_from_layer(tmp_blob, out_dir))
        finally:
            if os.path.exists(tmp_blob):
                os.remove(tmp_blob)

    if "gatehouse" not in found:
        raise RuntimeError("Artifact image %s:%s did not contain 'gatehouse' binary" % (image, resolved_tag))
    if "SHA256SUMS" not in found:
        raise RuntimeError("Artifact image %s:%s did not contain 'SHA256SUMS'" % (image, resolved_tag))

    actual_sha = sha256_file(found["gatehouse"])
    sums = parse_sha256sums(found["SHA256SUMS"])
    expected_sha = sums.get("gatehouse")
    if not expected_sha:
        raise RuntimeError("SHA256SUMS missing entry for 'gatehouse'")
    if actual_sha != expected_sha:
        raise RuntimeError(
            "gatehouse binary SHA256 mismatch against SHA256SUMS: expected %s, got %s"
            % (expected_sha, actual_sha)
        )

    meta = {}  # type: Dict[str, Any]
    meta_path = found.get("RELEASE_META", "")
    if meta_path:
        with open(meta_path, "r", encoding="utf-8") as fp:
            meta = json.load(fp)
        meta_sha = (meta.get("binary_sha256") or "").lower()
        if meta_sha and meta_sha != actual_sha:
            raise RuntimeError(
                "gatehouse binary SHA256 mismatch against RELEASE_META: expected %s, got %s"
                % (meta_sha, actual_sha)
            )

    version = str(meta.get("version") or resolved_tag)
    return {
        "image": "%s/%s" % (registry, repo),
        "tag": resolved_tag,
        "digest": digest,
        "version": version,
        "binary_sha256": actual_sha,
        "binary_path": found["gatehouse"],
        "sha256sums_path": found["SHA256SUMS"],
        "meta_path": meta_path,
    }


def main(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(
        description="Pull Precinct Gatehouse static binary artifact from OCI/Docker v2 registry"
    )
    parser.add_argument("--image", required=True, help="Registry/repo image without tag")
    parser.add_argument("--tag", default="latest", help="Image tag to pull (default: latest)")
    parser.add_argument("--username", default=os.environ.get("CN_REGISTRY_USERNAME", ""))
    parser.add_argument("--password", default=os.environ.get("CN_REGISTRY_PASSWORD", ""))
    parser.add_argument("--out-dir", default="", help="Output directory for gatehouse, SHA256SUMS, RELEASE_META")
    parser.add_argument("--check-only", action="store_true", help="Only fetch manifest digest without downloading blobs")
    parser.add_argument(
        "--dns",
        default=os.environ.get("MIRROR_DNS", ",".join(DEFAULT_DNS_SERVERS)),
        help="Comma-separated fallback DNS servers (default: 223.5.5.5,119.29.29.29)",
    )
    parser.add_argument("--timeout", type=int, default=60, help="HTTP timeout in seconds (default: 60)")
    parser.add_argument("--insecure-http", action="store_true", help="Use HTTP instead of HTTPS (testing only)")

    args = parser.parse_args(argv)

    dns_servers = tuple(s.strip() for s in args.dns.split(",") if s.strip())
    if dns_servers:
        install_fallback_dns(dns_servers)

    result = pull_artifact(
        image=args.image,
        tag=args.tag,
        username=args.username,
        password=args.password,
        out_dir=args.out_dir or None,
        check_only=args.check_only,
        timeout=args.timeout,
        insecure_http=args.insecure_http,
    )
    print(json.dumps(result, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:
        print("pull-from-registry error: %s" % exc, file=sys.stderr)
        sys.exit(1)
