"""Local-only video gateway integration, with fake engines and control plane."""

import base64
import contextlib
import http.client
import json
import os
import socket
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

ROOT = Path(os.environ.get("VIDEO_GATEWAY_TEST_DIR", "/task"))
A = "outbound|80||model-2-4.static"
B = "outbound|80||model-2-5.static"
CONTROL = "outbound|17803||control.dns"
REMOTE = "outbound|80||cluster-gateway.static"
JOBS = {}
REQUESTS = []
UNAVAILABLE = set()
BROKEN = set()
ASSET_TASKS = set()
RETIRED_TASKS = set()
PROFILE = None
DELETE_STYLE = "vllm"
DELETED = set()
VIDEO = b"\x00\x00\x00\x18ftypmp42" + b"video-test-" * 1000


class Engine(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def read_body(self):
        if self.headers.get("Transfer-Encoding", "").lower() != "chunked":
            return self.rfile.read(int(self.headers.get("Content-Length", 0)))
        parts = []
        while True:
            size = int(self.rfile.readline().split(b";", 1)[0], 16)
            if size == 0:
                self.rfile.readline()
                return b"".join(parts)
            parts.append(self.rfile.read(size))
            self.rfile.read(2)

    def do_POST(self):  # noqa: N802 - BaseHTTPRequestHandler dispatch contract
        body = self.read_body()
        REQUESTS.append((self.server.label, self.command, self.path, body))
        if b"drop-response" in body:
            self.connection.shutdown(socket.SHUT_RDWR)
            self.connection.close()
            return
        self.reply(200, {"id": "native-task", "status": "queued", "progress": 0})

    def do_GET(self):  # noqa: N802 - BaseHTTPRequestHandler dispatch contract
        REQUESTS.append((self.server.label, self.command, self.path, b""))
        if self.path.startswith("/files/video-content/"):
            self.send_response(200)
            self.send_header("Content-Type", "video/mp4")
            self.send_header("Content-Length", str(len(VIDEO)))
            self.end_headers()
            self.wfile.write(VIDEO)
            return
        if self.server.label in DELETED:
            self.reply(404, {"error": "video not found"})
            return
        if self.server.label in UNAVAILABLE:
            self.reply(503, {"error": "engine unavailable"})
            return
        if self.path.endswith("/content"):
            self.send_response(200)
            self.send_header("Content-Type", "video/mp4")
            self.send_header("Content-Length", str(len(VIDEO)))
            self.end_headers()
            self.wfile.write(VIDEO[:20])
            self.wfile.flush()
            if self.server.label in BROKEN:
                self.connection.shutdown(socket.SHUT_RDWR)
                self.connection.close()
                return
            time.sleep(0.03)
            self.wfile.write(VIDEO[20:])
            return
        self.reply(200, {"id": "native-task", "status": "completed", "progress": 100})

    def do_DELETE(self):  # noqa: N802 - BaseHTTPRequestHandler dispatch contract
        REQUESTS.append((self.server.label, self.command, self.path, b""))
        if self.server.label in UNAVAILABLE:
            self.reply(503, {"error": "engine unavailable"})
            return
        DELETED.add(self.server.label)
        self.reply(
            200,
            {
                "id": "native-task",
                **({"status": "deleted"} if DELETE_STYLE == "sglang" else {"deleted": True}),
            },
        )

    def reply(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


class Control(Engine):
    def do_POST(self):  # noqa: N802 - BaseHTTPRequestHandler dispatch contract
        body = json.loads(self.read_body() or b"{}")
        assert set(body) <= {
            "model",
            "destination",
            "native_id",
            "native_model",
            "task_id",
            "ticket",
            "request_parameters",
            "operation",
            "job",
            "keep_task",
        }, body
        if self.path.endswith("/prepare"):
            assert body["model"] == "published-wan" and body["native_model"] == "engine-wan", body
            assert body["request_parameters"]["model"] == "published-wan"
            task = "video_" + format(len(JOBS) + 1, "032x")
            dest = body["destination"]
            assert dest in ("model-2-4.static", "model-2-5.static", "cluster-gateway.static"), dest
            cluster = (
                REMOTE
                if dest == "cluster-gateway.static"
                else A
                if dest == "model-2-4.static"
                else B
            )
            JOBS[task] = {
                "id": task,
                "model": "published-wan",
                "cluster": cluster,
                "destination": dest,
                "native_model": "engine-wan",
                "path": "/v1/videos/native-task",
                "bound": cluster != REMOTE,
                "capabilities": media_capabilities(),
            }
            if cluster == REMOTE:
                self.reply(
                    200,
                    {
                        **JOBS[task],
                        "forward": True,
                        "ticket": task + "-grant",
                        "path": "/v1/videos/" + task,
                    },
                )
            else:
                self.reply(200, JOBS[task])
        elif self.path.endswith("/relay/bind"):
            task = body["task_id"]
            binding = JOBS[task]
            if binding.get("bound"):
                self.reply(503, {})
                return
            dest = body["destination"]
            assert dest in ("model-2-4.static", "model-2-5.static"), dest
            binding["bound"] = True
            binding["inner"] = {
                **binding,
                "model": "engine-wan",
                "capabilities": media_capabilities(),
                "destination": dest,
                "cluster": A if dest == "model-2-4.static" else B,
            }
            self.reply(200, binding["inner"])
        elif self.path.endswith("/relay/resolve"):
            self.reply(
                200,
                asset_binding(
                    JOBS[body["task_id"]]["inner"], body["task_id"], body.get("operation", "read")
                ),
            )
        elif self.path.endswith("/delete-started") or self.path.endswith("/deleted"):
            task = body.get("task_id", self.path.split("/")[-2])
            binding = JOBS[task]
            expected = binding.get("inner", binding)
            assert body["native_id"] == "native-task"
            assert body["destination"] == expected["destination"]
            binding["deletion_state"] = "confirmed" if self.path.endswith("/deleted") else "unknown"
            binding["keep_task"] = body.get("keep_task", False)
            self.reply(
                200, deletion_result(task) if self.path.endswith("/deleted") else {"id": task}
            )
        elif self.path.split("?", 1)[0].endswith("/deletion-confirmed"):
            task = self.path.split("/")[-2]
            assert JOBS[task]["deletion_state"] == "confirmed"
            self.reply(200, deletion_result(task))
        else:
            self.reply(200, {"id": body.get("task_id", self.path.split("/")[-2])})

    def do_GET(self):  # noqa: N802 - BaseHTTPRequestHandler dispatch contract
        task = self.path.split("/")[-2]
        if task not in JOBS:
            self.reply(404, {"error": "unknown task"})
            return
        binding = JOBS[task]
        if binding.get("deletion_state") == "confirmed":
            self.reply(404, {"error": "deleted task"})
            return
        if binding["cluster"] == REMOTE:
            binding = {
                **binding,
                "forward": True,
                "ticket": task + ("-delete" if "operation=delete" in self.path else "-read"),
                "path": "/v1/videos/" + task,
            }
        self.reply(
            200,
            asset_binding(binding, task, "content" if "operation=content" in self.path else "read"),
        )


def deletion_result(task):
    cleanup = "retained" if DELETE_STYLE == "sglang" else "unconfirmed"
    if JOBS[task].get("keep_task"):
        return {
            "id": task,
            "object": "video",
            "status": "cancelled",
            "record_only_delete": True,
            "file_cleanup": cleanup,
        }
    return {
        "id": task,
        "object": "video.deleted",
        "deleted": True,
        "record_deleted": True,
        "file_cleanup": cleanup,
    }


def asset_binding(binding, task, operation):
    if task in RETIRED_TASKS and operation == "read" and not binding.get("forward"):
        return {
            "id": task,
            "model": binding["model"],
            "status_only": True,
            "response": {"id": task, "status": "failed", "error": {"code": "video_runtime_lost"}},
        }
    if task not in ASSET_TASKS or binding.get("forward"):
        return binding
    result = {
        **binding,
        "asset": True,
        "path": "/files/video-content/" + task,
        "headers": {"x-gpustack-video-file-grant": "test-file-grant"},
    }
    if operation == "read":
        result["response"] = {"id": task, "status": "completed", "file_state": "ready"}
    return result


def media_capabilities():
    if PROFILE is None:
        return None
    return {
        "request_profile": PROFILE,
        "tasks": ["fl2va"],
        "default_task": "fl2va",
        "default_size": "768x1344",
        "media_inputs": {"fl2va": {"keyframes": True, "image": 2, "max_items": 2}},
    }


def serve(port, handler, label="control"):
    server = ThreadingHTTPServer(("127.0.0.1", port), handler)
    server.label = label
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


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


def config(lb, role="edge", remote=False):
    def route(name, match, action):
        return {
            "name": name,
            "match": match,
            "route": {
                **action,
                "timeout": "20s",
                "retry_policy": {"retry_on": "", "num_retries": 0},
            },
        }

    model = [
        {
            "name": "x-higress-llm-model",
            "string_match": {"exact": "published-wan" if role == "edge" else "engine-wan"},
        }
    ]
    choices = (
        [{"name": REMOTE, "weight": 100}]
        if remote
        else [{"name": A, "weight": 50}, {"name": B, "weight": 50}]
    )
    create_action = (
        {"cluster_header": "x-higress-target-cluster"}
        if lb
        else {"weighted_clusters": {"clusters": choices}}
    )
    routes = [
        route(
            "bootstrap",
            {
                "prefix": "/v1/videos/video_",
                "headers": [{"name": "x-gpustack-video-bootstrap", "string_match": {"exact": "1"}}],
            },
            {"cluster": CONTROL},
        ),
        route(
            "read",
            {"prefix": "/v1/videos/video_", "headers": model},
            {"cluster_header": "x-gpustack-video-cluster"},
        ),
        route("create", {"path": "/v1/videos", "headers": model}, create_action),
        {"name": "unknown", "match": {"prefix": "/"}, "direct_response": {"status": 404}},
    ]
    control_cfg = {"serviceName": "control.dns", "servicePort": 17803, "gatewayToken": "local-only"}
    filters = [
        wasm(
            "model-router",
            "router.wasm",
            {
                "videoJobs": True,
                "videoRole": role,
                "enableOnPathSuffix": ["/v1/videos"],
                "aliasNameMapping": {"9": "published-wan" if role == "edge" else "engine-wan"},
            },
        ),
        wasm("resolve", "video.wasm", {**control_cfg, "mode": "resolve", "role": role}),
    ]
    if lb:
        filters += [
            wasm(
                "lb-context",
                "lb.wasm",
                {
                    "mode": "context",
                    "_rules_": [
                        {
                            "_match_route_": ["create"],
                            "candidates": [
                                {"cluster": item["name"], "targetId": "2", "kind": "instance"}
                                for item in choices
                            ],
                            "modelMappers": {"2": {"*": "engine-wan"}},
                        }
                    ],
                },
            ),
            wasm(
                "lb-finisher", "lb.wasm", {"mode": "finisher", "enableOnPathSuffix": ["/v1/videos"]}
            ),
        ]
    else:
        filters += [
            wasm(
                "mapper",
                "lb.wasm",
                {
                    "mode": "context",
                    "enableOnPathSuffix": ["/v1/videos"],
                    "_rules_": [
                        {
                            "_match_route_": ["create"],
                            "modelMapping": {"published-wan": "engine-wan"},
                        }
                    ],
                },
            )
        ]
    filters += [
        wasm(
            "ai-proxy",
            "ai-proxy.wasm",
            {
                "provider": {
                    "type": "openai",
                    "apiTokens": ["local-test-one", "local-test-two"],
                    "retryOnFailure": {"enabled": True},
                }
            },
        ),
        wasm(
            "pre-route",
            "pre-route.wasm",
            {
                "clusterNameHeader": "x-gpustack-model-instance",
                "enableOnPathSuffix": ["/videos"],
                "enableOnPathPrefix": ["/v1/videos/"],
            },
        ),
        wasm("transport", "video.wasm", {**control_cfg, "mode": "transport", "role": role}),
        {
            "name": "envoy.filters.http.router",
            "typed_config": {
                "@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"
            },
        },
    ]
    for item in filters:
        if item["name"] == "envoy.filters.http.wasm":
            item["typed_config"]["config"]["root_id"] = (
                role + "-" + item["typed_config"]["config"]["root_id"]
            )

    def cluster(name, port):
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
                                    "address": {
                                        "socket_address": {
                                            "address": "127.0.0.1",
                                            "port_value": port,
                                        }
                                    }
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
                    "name": "test-" + role,
                    "address": {
                        "socket_address": {
                            "address": "127.0.0.1",
                            "port_value": 17805 if role == "cluster" else 17800,
                        }
                    },
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
            "clusters": [
                cluster(A, 17801),
                cluster(B, 17802),
                cluster(CONTROL, 17803),
                cluster(REMOTE, 17805),
            ],
        }
    }


def request(method, path, body=None, headers=None):
    c = http.client.HTTPConnection("127.0.0.1", 17800, timeout=15)
    data = None if body is None else body if isinstance(body, bytes) else json.dumps(body).encode()
    c.request(method, path, data, {"Content-Type": "application/json", **(headers or {})})
    try:
        response = c.getresponse()
        return response.status, response.read()
    finally:
        c.close()


def main():
    global PROFILE, DELETE_STYLE
    servers = [serve(17801, Engine, "A"), serve(17802, Engine, "B"), serve(17803, Control)]
    try:
        for lb, inner_lb, remote in (
            (False, False, False),
            (True, False, False),
            (False, False, True),
            (False, True, True),
            (True, False, True),
            (True, True, True),
        ):
            path = ROOT / ("envoy-lb-on.json" if lb else "envoy-lb-off.json")
            assembled = config(lb, remote=remote)
            if remote:
                assembled["static_resources"]["listeners"] += config(inner_lb, role="cluster")[
                    "static_resources"
                ]["listeners"]
            path.write_text(json.dumps(assembled))
            with (ROOT / ("envoy-lb-on.log" if lb else "envoy-lb-off.log")).open("w") as log:
                process = subprocess.Popen(
                    [
                        os.environ.get("ENVOY_BINARY", "/usr/local/bin/envoy"),
                        "-c",
                        str(path),
                        "--concurrency",
                        "1",
                        "--log-level",
                        "warn",
                    ],
                    stdout=log,
                    stderr=log,
                )
                try:
                    for _ in range(450):
                        if process.poll() is not None:
                            raise RuntimeError("Envoy exited; check log")
                        try:
                            with socket.create_connection(("127.0.0.1", 17800), timeout=0.1):
                                break
                        except OSError:
                            time.sleep(0.1)
                    for _ in range(5):
                        status, body = request(
                            "POST", "/v1/videos", {"model": "published-wan", "prompt": "test"}
                        )
                        assert status == 200, (status, body)
                        task = json.loads(body)["id"]
                        target = JOBS[task]["destination"]
                        if remote:
                            target = JOBS[task]["inner"]["destination"]
                        label = "A" if target == "model-2-4.static" else "B"
                        status, body = request(
                            "GET",
                            "/v1/videos/" + task,
                            headers={
                                "x-gpustack-video-cluster": B if label == "A" else A,
                                "x-gpustack-model": "forged",
                            },
                        )
                        assert status == 200 and json.loads(body)["id"] == task, (status, body)
                        assert REQUESTS[-1][:3] == (label, "GET", "/v1/videos/native-task"), (
                            REQUESTS[-1]
                        )
                        status, body = request("GET", "/v1/videos/" + task + "/content")
                        assert status == 200 and body == VIDEO, (status, body[:100])
                        assert REQUESTS[-1][0] == label, REQUESTS[-1]
                    RETIRED_TASKS.add(task)
                    previous_requests = len(REQUESTS)
                    status, body = request("GET", "/v1/videos/" + task)
                    assert status == 200 and json.loads(body)["status"] == "failed", (status, body)
                    assert len(REQUESTS) == previous_requests, "retired task contacted an engine"
                    RETIRED_TASKS.remove(task)
                    for create_path in (
                        "/model/proxy/9/v1/videos",
                        "/model/proxy/v1/videos",
                        "/V1/VIDEOS",
                        "/MODEL/PROXY/9/V1/VIDEOS/",
                    ):
                        status, body = request(
                            "POST", create_path, {"model": "published-wan", "prompt": "alias"}
                        )
                        assert status == 200, (create_path, status, body)
                        alias_task = json.loads(body)["id"]
                        for read_path in (
                            "/model/proxy/9/v1/videos/" + alias_task,
                            "/V1/VIDEOS/" + alias_task,
                        ):
                            status, body = request("GET", read_path)
                            assert status == 200 and json.loads(body)["id"] == alias_task, (
                                read_path,
                                status,
                                body,
                            )
                            status, body = request("GET", read_path + "/content")
                            assert status == 200 and body == VIDEO, (read_path, status, body[:100])
                    before = len(REQUESTS)
                    for raw_path in (
                        "/model/proxy/9/v1/videos",
                        "/model/proxy/9/v1/videos/native-task",
                        "/model/proxy/9/v1/videos/native-task/content",
                    ):
                        status, _ = request(
                            "GET", raw_path, headers={"x-higress-llm-model": "published-wan"}
                        )
                        assert status >= 400 and len(REQUESTS) == before, (raw_path, status)
                    status, _ = request(
                        "GET",
                        "/v1/chat/completions",
                        headers={"x-gpustack-fallback-path": "/v1/videos/native-task"},
                    )
                    assert status == 404 and len(REQUESTS) == before
                    ASSET_TASKS.add(task)
                    UNAVAILABLE.add(label)
                    before = len(REQUESTS)
                    status, body = request("GET", "/v1/videos/" + task)
                    assert status == 200 and json.loads(body)["file_state"] == "ready", (
                        status,
                        body,
                    )
                    assert len(REQUESTS) == before, "completed file status contacted the engine"
                    status, body = request("GET", "/v1/videos/" + task + "/content")
                    assert status == 200 and body == VIDEO, (status, body[:100])
                    assert REQUESTS[-1][:3] == (label, "GET", "/files/video-content/" + task), (
                        REQUESTS[-1]
                    )
                    ASSET_TASKS.discard(task)
                    UNAVAILABLE.clear()
                    UNAVAILABLE.add(label)
                    for suffix in ("", "/content"):
                        before = len(REQUESTS)
                        status, _ = request("GET", "/v1/videos/" + task + suffix)
                        assert (
                            status == 503
                            and len(REQUESTS) == before + 1
                            and REQUESTS[-1][0] == label
                        )
                    UNAVAILABLE.clear()
                    BROKEN.add(label)
                    before = len(REQUESTS)
                    try:
                        request("GET", "/v1/videos/" + task + "/content")
                        raise AssertionError("truncated MP4 was treated as a successful response")
                    except http.client.IncompleteRead as error:
                        assert error.partial == VIDEO[:20], error.partial
                    finally:
                        BROKEN.clear()
                    assert len(REQUESTS) == before + 1 and REQUESTS[-1][0] == label
                    for PROFILE in ("sglang_h3", "vllm_omni_h3"):
                        boundary = "video-test-boundary"
                        fields = {
                            "model": "published-wan",
                            "prompt": "test",
                            "seconds": "4",
                            "size": "720x1280",
                            "media_inputs": json.dumps(
                                [
                                    {
                                        "type": "image",
                                        "role": "keyframe",
                                        "frame_index": 0,
                                        "upload_field": "first",
                                    }
                                ]
                            ),
                        }
                        parts = []
                        for key, value in fields.items():
                            parts.append(
                                (
                                    "--"
                                    + boundary
                                    + '\r\nContent-Disposition: form-data; name="'
                                    + key
                                    + '"\r\n\r\n'
                                    + value
                                    + "\r\n"
                                ).encode()
                            )
                        parts.append(
                            (
                                "--"
                                + boundary
                                + '\r\nContent-Disposition: form-data; name="first"; filename="first.png"\r\nContent-Type: image/png\r\n\r\n'
                            ).encode()
                            + b"first-frame-bytes"
                            + b"\r\n"
                        )
                        parts.append(("--" + boundary + "--\r\n").encode())
                        status, body = request(
                            "POST",
                            "/v1/videos",
                            b"".join(parts),
                            {"Content-Type": "multipart/form-data; boundary=" + boundary},
                        )
                        assert status == 200, (status, body)
                        upstream = REQUESTS[-1][3]
                        assert (
                            b"media_inputs" not in upstream
                            and base64.b64encode(b"first-frame-bytes") in upstream
                        )
                        assert (
                            b"conditions" if PROFILE == "sglang_h3" else b"image_reference"
                        ) in upstream
                    PROFILE = None
                    before = len(REQUESTS)
                    status, body = request(
                        "POST",
                        "/v1/videos",
                        {"model": "published-wan", "prompt": "drop-response"},
                        headers={"x-envoy-retry-on": "5xx,reset", "x-envoy-max-retries": "5"},
                    )
                    assert status >= 500, (status, body)
                    assert len(REQUESTS) == before + 1, REQUESTS[before:]
                    before = len(REQUESTS)
                    status, _ = request("GET", "/v1/videos/video_" + "f" * 32)
                    assert status == 404 and len(REQUESTS) == before, status
                    status, _ = request(
                        "GET", "/v1/videos", headers={"x-higress-llm-model": "published-wan"}
                    )
                    assert status == 404 and len(REQUESTS) == before, status
                    for DELETE_STYLE in ("sglang", "vllm"):
                        status, body = request(
                            "POST",
                            "/v1/videos",
                            {"model": "published-wan", "prompt": "delete test"},
                        )
                        assert status == 200, (status, body)
                        delete_id = json.loads(body)["id"]
                        delete_binding = JOBS[delete_id].get("inner", JOBS[delete_id])
                        delete_label = (
                            "A" if delete_binding["destination"] == "model-2-4.static" else "B"
                        )
                        UNAVAILABLE.add(delete_label)
                        before = len(REQUESTS)
                        status, _ = request(
                            "DELETE",
                            "/v1/videos/" + delete_id,
                            headers={"x-envoy-max-retries": "5", "x-envoy-retry-on": "5xx"},
                        )
                        assert status == 503 and len(REQUESTS) == before + 1
                        assert JOBS[delete_id]["deletion_state"] == "unknown"
                        UNAVAILABLE.clear()
                        before = len(REQUESTS)
                        status, body = request("DELETE", "/MODEL/PROXY/9/v1/videos/" + delete_id)
                        assert status == 200 and json.loads(body) == {
                            "id": delete_id,
                            "object": "video.deleted",
                            "deleted": True,
                            "record_deleted": True,
                            "file_cleanup": (
                                "retained" if DELETE_STYLE == "sglang" else "unconfirmed"
                            ),
                        }, (status, body)
                        assert REQUESTS[-1][:3] == (
                            delete_label,
                            "DELETE",
                            "/v1/videos/native-task",
                        )
                        assert (
                            len(REQUESTS) == before + 1
                            and JOBS[delete_id]["deletion_state"] == "confirmed"
                        )
                        before = len(REQUESTS)
                        status, _ = request("GET", "/v1/videos/" + delete_id)
                        assert status == 404 and len(REQUESTS) == before
                        DELETED.clear()
                    status, body = request(
                        "POST",
                        "/v1/videos",
                        {"model": "published-wan", "prompt": "cancel while retaining record"},
                    )
                    assert status == 200, (status, body)
                    cancel_id = json.loads(body)["id"]
                    status, body = request("DELETE", "/v1/videos/" + cancel_id + "?keep_task=true")
                    assert status == 200 and json.loads(body) == {
                        "id": cancel_id,
                        "object": "video",
                        "status": "cancelled",
                        "record_only_delete": True,
                        "file_cleanup": "unconfirmed",
                    }, (status, body)
                    assert (
                        JOBS[cancel_id]["deletion_state"] == "confirmed"
                        and JOBS[cancel_id]["keep_task"] is True
                    )
                    assert REQUESTS[-1][1:3] == ("DELETE", "/v1/videos/native-task"), REQUESTS[-1]
                    DELETED.clear()
                    print(
                        json.dumps(
                            {
                                "custom_lb": lb,
                                "inner_lb": inner_lb,
                                "remote": remote,
                                "status": "passed",
                                "engine_requests": len(REQUESTS),
                            }
                        ),
                        flush=True,
                    )
                finally:
                    process.terminate()
                    with contextlib.suppress(subprocess.TimeoutExpired):
                        process.wait(timeout=5)
                    if process.poll() is None:
                        process.kill()
                        process.wait()
    finally:
        for server in servers:
            server.shutdown()


if __name__ == "__main__":
    main()
