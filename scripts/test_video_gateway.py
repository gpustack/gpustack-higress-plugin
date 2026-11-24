"""Run native video routing against Envoy and local fake engines."""

import contextlib
import http.client
import json
import os
import socket
import subprocess
import threading
import time
from email import policy
from email.parser import BytesParser
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

ROOT = Path(os.environ.get("VIDEO_GATEWAY_TEST_DIR", "/task"))
ENGINE = "outbound|80||model-2-4.static"
REMOTE = "outbound|80||cluster-gateway.static"
NATIVE_ID = "native-engine-task"
VIDEO = b"\x00\x00\x00\x18ftypmp42" + b"video-test-" * 1000
ROUTE_MODEL = {"edge": "published-wan", "cluster": "engine-wan"}
CALLS = []
DESTINATIONS = []


class Engine(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def read_body(self):
        if self.headers.get("Transfer-Encoding", "").lower() != "chunked":
            return self.rfile.read(int(self.headers["Content-Length"]))
        chunks = []
        while length := int(self.rfile.readline().split(b";", 1)[0], 16):
            chunks.append(self.rfile.read(length))
            self.rfile.read(2)
        return b"".join(chunks)

    def do_POST(self):  # noqa: N802
        DESTINATIONS.append(self.headers.get("X-GPUStack-Model-Instance"))
        body = self.read_body()
        if self.path == "/v1/videos/sync":
            content_type = self.headers["Content-Type"]
            envelope = f"Content-Type: {content_type}\r\n\r\n".encode()
            multipart = BytesParser(policy=policy.default).parsebytes(envelope + body)
            fields = {
                part.get_param("name", header="content-disposition"): part.get_content()
                for part in multipart.iter_parts()
            }
            CALLS.append((self.command, self.path, fields))
            self.reply(VIDEO, "video/mp4")
            return
        CALLS.append((self.command, self.path, json.loads(body)))
        if b"drop-response" in body:
            self.connection.shutdown(socket.SHUT_RDWR)
            self.connection.close()
            return
        self.reply({"id": NATIVE_ID, "status": "queued"})

    def do_GET(self):  # noqa: N802
        DESTINATIONS.append(self.headers.get("X-GPUStack-Model-Instance"))
        CALLS.append((self.command, self.path, None))
        if self.path == f"/v1/videos/{NATIVE_ID}/content":
            self.reply(VIDEO, "video/mp4")
        else:
            self.reply({"id": NATIVE_ID, "status": "completed", "progress": 100})

    def do_DELETE(self):  # noqa: N802
        DESTINATIONS.append(self.headers.get("X-GPUStack-Model-Instance"))
        CALLS.append((self.command, self.path, None))
        self.reply({"id": NATIVE_ID, "status": "deleted"})

    def reply(self, body, content_type="application/json"):
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def wasm(name, filename, config):
    return {
        "name": "envoy.filters.http.wasm",
        "typed_config": {
            "@type": "type.googleapis.com/envoy.extensions.filters.http.wasm.v3.Wasm",
            "config": {
                "name": name,
                "root_id": name,
                "vm_config": {
                    "vm_id": filename,
                    "runtime": "envoy.wasm.runtime.v8",
                    "code": {"local": {"filename": str(ROOT / "wasm" / filename)}},
                },
                "configuration": {
                    "@type": "type.googleapis.com/google.protobuf.StringValue",
                    "value": json.dumps(config),
                },
            },
        },
    }


def gateway(role, lb=False, remote=False, h3=False):
    port = 17800 if role == "edge" else 17805
    cluster = REMOTE if remote else ENGINE
    model = ROUTE_MODEL[role]
    action = {"cluster_header": "x-higress-target-cluster"} if lb else {"cluster": cluster}
    routes = [
        {
            "name": name,
            "match": {
                **path,
                "headers": [
                    {"name": "x-higress-llm-model", "string_match": {"exact": model}},
                    {"name": ":method", "string_match": {"safe_regex": {"regex": methods}}},
                ],
            },
            "route": {**action, "timeout": "10s"},
        }
        for name, path, methods in [
            ("create", {"path": "/v1/videos"}, "POST"),
            ("read", {"prefix": "/v1/videos/"}, "GET|DELETE"),
            ("sync", {"path": "/v1/videos/sync"}, "POST"),
        ]
    ]
    filters = [
        wasm(
            "model-router",
            "router.wasm",
            {
                "videoRoutes": True,
                "enableOnPathSuffix": ["/v1/videos", "/v1/videos/sync"],
                "aliasNameMapping": {"9": model},
            },
        ),
    ]
    if lb:
        filters.extend(
            [
                wasm(
                    "lb-context",
                    "lb.wasm",
                    {
                        "mode": "context",
                        "_rules_": [
                            {
                                "_match_route_": ["create", "read", "sync"],
                                "candidates": [
                                    {"cluster": cluster, "targetId": "2", "kind": "instance"}
                                ],
                                "modelMappers": {"2": {model: "engine-wan"}},
                            }
                        ],
                    },
                ),
                wasm(
                    "lb-finisher",
                    "lb.wasm",
                    {"mode": "finisher", "enableOnPathSuffix": ["/v1/videos/sync"]},
                ),
            ]
        )
    else:
        filters.append(
            wasm(
                "model-mapper",
                "mapper.wasm",
                {
                    "enableOnPathSuffix": ["/v1/videos/sync"],
                    "modelMapping": {model: "engine-wan"},
                },
            )
        )
    target = {"model": "engine-wan", "remote": remote}
    if h3 and not remote:
        target["capabilities"] = {
            "request_profile": "sglang_h3",
            "tasks": ["t2va"],
            "default_task": "t2va",
            "default_size": "768x1344",
        }
    # A remote gateway derives its own instance header.
    if not remote:
        filters.append(
            wasm(
                "pre-route",
                "pre-route.wasm",
                {
                    "clusterNameHeader": "X-GPUStack-Model-Instance",
                    "enableOnPathSuffix": ["/videos"],
                    "enableOnPathPrefix": ["/v1/videos/"],
                },
            )
        )
    filters.extend(
        [
            wasm(
                "video",
                "video.wasm",
                {
                    "_rules_": [
                        {
                            "_match_route_": ["create", "read"],
                            "targets": {cluster: target},
                        }
                    ]
                },
            ),
            {
                "name": "envoy.filters.http.router",
                "typed_config": {
                    "@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router",
                },
            },
        ]
    )
    routes.append(
        {
            "match": {"prefix": "/"},
            "direct_response": {"status": 404},
        }
    )

    def upstream(name, endpoint):
        return {
            "name": name,
            "type": "STATIC",
            "connect_timeout": "1s",
            "load_assignment": {
                "cluster_name": name,
                "endpoints": [
                    {
                        "lb_endpoints": [
                            {
                                "endpoint": {
                                    "address": address(endpoint),
                                }
                            }
                        ]
                    }
                ],
            },
        }

    return {
        "static_resources": {
            "listeners": [
                {
                    "name": role,
                    "address": address(port),
                    "filter_chains": [
                        {
                            "filters": [
                                {
                                    "name": "envoy.filters.network.http_connection_manager",
                                    "typed_config": {
                                        "@type": "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
                                        "stat_prefix": "video_test",
                                        "route_config": {
                                            "name": "video_routes",
                                            "virtual_hosts": [
                                                {"name": "all", "domains": ["*"], "routes": routes}
                                            ],
                                        },
                                        "http_filters": filters,
                                    },
                                }
                            ]
                        }
                    ],
                }
            ],
            "clusters": [upstream(ENGINE, 17801), upstream(REMOTE, 17805)],
        }
    }


def address(port):
    return {"socket_address": {"address": "127.0.0.1", "port_value": port}}


def wait_for_port(port, process):
    deadline = time.monotonic() + 30
    while process.poll() is None and time.monotonic() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                return
        except OSError:
            time.sleep(0.05)
    raise RuntimeError(f"Envoy did not start on port {port}. See the gateway log.")


@contextlib.contextmanager
def envoy(role, **kwargs):
    path = ROOT / f"envoy-{role}.json"
    path.write_text(json.dumps(gateway(role, **kwargs)))
    with (ROOT / f"envoy-{role}.log").open("w") as log:
        process = subprocess.Popen(
            [
                os.environ.get("ENVOY_BINARY", "/usr/local/bin/envoy"),
                "-c",
                str(path),
                "--concurrency",
                "1",
                "--base-id",
                "5" if role == "edge" else "10",
            ],
            stdout=log,
            stderr=log,
        )
        try:
            wait_for_port(17800 if role == "edge" else 17805, process)
            yield
        finally:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)


def request(method, path, body=None, headers=None):
    conn = http.client.HTTPConnection("127.0.0.1", 17800, timeout=12)
    data = json.dumps(body).encode() if isinstance(body, dict) else body
    try:
        conn.request(method, path, data, {"Content-Type": "application/json", **(headers or {})})
        response = conn.getresponse()
        return response.status, response.read()
    finally:
        conn.close()


def check_round_trip(h3):
    CALLS.clear()
    DESTINATIONS.clear()
    status, body = request(
        "POST",
        "/v1/videos",
        {
            "model": "published-wan",
            "prompt": "Test video",
            "seconds": 4,
            "size": "832x480",
        },
    )
    assert status == 200 and json.loads(body)["id"] == NATIVE_ID, (status, body)
    sent = CALLS[0][2]
    assert sent["model"] == "engine-wan", sent
    if h3:
        assert sent["task"] == "t2va" and sent["target"]["short_edge"] == 480, sent

    headers = {"X-GPUStack-Model": "published-wan"}
    path = f"/v1/videos/{NATIVE_ID}"
    status, body = request("GET", path, headers=headers)
    assert status == 200 and json.loads(body)["status"] == "completed", (status, body)
    assert request("GET", path + "/content", headers=headers) == (200, VIDEO)
    status, body = request("DELETE", path, headers=headers)
    assert status == 200 and json.loads(body) == {"id": NATIVE_ID, "status": "deleted"}
    assert [call[:2] for call in CALLS] == [
        ("POST", "/v1/videos"),
        ("GET", path),
        ("GET", path + "/content"),
        ("DELETE", path),
    ]
    assert DESTINATIONS == [ENGINE.split("|")[-1]] * 4, DESTINATIONS
    boundary = "sync-video-test"
    payload = (
        f'--{boundary}\r\nContent-Disposition: form-data; name="model"\r\n\r\n'
        f"published-wan\r\n--{boundary}\r\n"
        'Content-Disposition: form-data; name="prompt"\r\n\r\n'
        f"Test synchronous video\r\n--{boundary}--\r\n"
    ).encode()
    assert request(
        "POST",
        "/v1/videos/sync",
        payload,
        {"Content-Type": f"multipart/form-data; boundary={boundary}"},
    ) == (200, VIDEO)
    assert CALLS[-1] == (
        "POST",
        "/v1/videos/sync",
        {"model": "engine-wan", "prompt": "Test synchronous video"},
    )
    for method, invalid in [
        ("GET", "/v1/videos"),
        ("GET", "/v1/videos/sync"),
        ("POST", path + "/remix"),
        ("GET", "/v2/video-tasks"),
    ]:
        assert request(method, invalid, headers=headers)[0] == 404
    assert request("GET", path)[0] == 400
    assert len(CALLS) == 5


def main():
    with ThreadingHTTPServer(("127.0.0.1", 17801), Engine) as engine:
        thread = threading.Thread(target=engine.serve_forever, daemon=True)
        thread.start()
        try:
            for lb in (False, True):
                with envoy("edge", lb=lb, h3=True):
                    check_round_trip(h3=True)
                    before = len(CALLS)
                    status, _ = request(
                        "POST",
                        "/v1/videos",
                        {
                            "model": "published-wan",
                            "prompt": "drop-response",
                            "seconds": 4,
                        },
                        {"x-envoy-retry-on": "reset,5xx", "x-envoy-max-retries": "5"},
                    )
                    assert status >= 500 and len(CALLS) == before + 1
                print(f"Envoy local round trip passed (LB={lb})")
            with envoy("cluster", h3=True), envoy("edge", remote=True):
                check_round_trip(h3=True)
            print("Envoy remote gateway round trip passed")
        finally:
            engine.shutdown()
            thread.join(timeout=2)


if __name__ == "__main__":
    main()
