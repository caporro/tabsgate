#!/usr/bin/env python3
"""Exercise the built image against a local fake provider. No paid API calls."""
import base64
import http.server
import json
import pathlib
import secrets
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
import uuid


def docker(*args):
    return subprocess.check_output(["docker", *args], text=True).strip()


class Provider(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        assert self.path == "/v1/models"
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps({"data": [{"id": "test-model"}, {"id": "denied"}]}).encode())

    def do_POST(self):
        assert self.path == "/v1/chat/completions"
        assert self.headers.get("Authorization") == "Bearer smoke-provider-secret"
        assert not self.headers.get("X-TabsGate-Keyword")
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        assert body["max_completion_tokens"] <= 100
        payload = {"choices": [{"message": {"role": "assistant", "content": "ok"}}],
                   "usage": {"prompt_tokens": 40, "completion_tokens": 35, "total_tokens": 75}}
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream" if body.get("stream") else "application/json")
        self.end_headers()
        if body.get("stream"):
            assert body["stream_options"]["include_usage"]
            self.wfile.write(b'data: {"choices":[{"delta":{"content":"ok"}}]}\n\n')
            self.wfile.flush()
            self.wfile.write(b"data: " + json.dumps(payload).encode() + b"\n\ndata: [DONE]\n\n")
        else:
            self.wfile.write(json.dumps(payload).encode())


def main():
    suffix = uuid.uuid4().hex[:12]
    name = "tabsgate-smoke-" + suffix
    volume = name + "-data"
    provider = http.server.ThreadingHTTPServer(("0.0.0.0", 0), Provider)
    threading.Thread(target=provider.serve_forever, daemon=True).start()
    created = False
    docker("volume", "create", volume)
    try:
        with tempfile.TemporaryDirectory(prefix="tabsgate-smoke-") as tmp:
            root = pathlib.Path(tmp)
            (root / "key").write_text(base64.b64encode(secrets.token_bytes(32)).decode())
            (root / "providers.json").write_text(json.dumps([{
                "id": "test", "name": "Test", "base_url": f"http://host.docker.internal:{provider.server_port}/v1",
                "output_limit_field": "max_completion_tokens"}]))
            # These are disposable test credentials, readable by the non-root container.
            for file in root.iterdir():
                file.chmod(0o444)
            docker("run", "-d", "--name", name, "--read-only", "--cap-drop", "ALL",
                   "--security-opt", "no-new-privileges", "--memory", "512m",
                   "--add-host", "host.docker.internal:host-gateway",
                   "-p", "127.0.0.1::8080", "-v", volume + ":/data",
                   "-v", str(root / "key") + ":/run/secrets/master_key:ro",
                   "-v", str(root / "providers.json") + ":/providers.json:ro",
                   "-e", "TABSGATE_MASTER_KEY_FILE=/run/secrets/master_key",
                   "-e", "TABSGATE_PROVIDERS_FILE=/providers.json",
                   "-e", "TABSGATE_ALLOW_PRIVATE_UPSTREAMS=true", "tabsgate:local")
            created = True

            def base_url():
                return "http://127.0.0.1:" + docker("port", name, "8080/tcp").rsplit(":", 1)[1]

            base = base_url()

            def request(method, path, data=None, token=None, admin=False, expected=200):
                headers = {"Content-Type": "application/json"}
                if admin:
                    headers.update({"Authorization": "Bearer smoke-provider-secret", "X-TabsGate-Keyword": "smoke-word"})
                elif token:
                    headers["Authorization"] = "Bearer " + token
                req = urllib.request.Request(base + path, data=json.dumps(data).encode() if data is not None else None,
                                             method=method, headers=headers)
                try:
                    response = urllib.request.urlopen(req, timeout=10)
                except urllib.error.HTTPError as exc:
                    response = exc
                with response:
                    body = response.read()
                    assert response.code == expected, (method, path, response.code, body)
                    if "text/event-stream" in response.headers.get("Content-Type", ""):
                        assert b"[DONE]" in body and b'"error"' not in body
                        return body
                    return json.loads(body) if body else None

            def ready():
                for _ in range(120):
                    try:
                        request("GET", "/readyz")
                        return
                    except (OSError, AssertionError):
                        time.sleep(0.25)
                raise RuntimeError("Container did not become ready")

            ready()
            print("Container ready (non-root, read-only root, persistent volume).", flush=True)
            conn = request("POST", "/api/v1/connections", {"provider": "test", "api_key": "smoke-provider-secret", "keyword": "smoke-word"}, expected=201)
            path = "/api/v1/connections/" + conn["id"]

            def issue(max_requests=30, token_budget=100):
                return request("POST", path + "/keys", {"name": "smoke", "max_requests": max_requests,
                               "token_budget": token_budget, "ttl_seconds": 3600,
                               "allowed_models": ["test-model"], "max_output_tokens": 100}, admin=True, expected=201)

            key = issue()
            models = request("GET", "/v1/models", token=key["key"])
            assert [m["id"] for m in models["data"]] == ["test-model"]
            chat = {"model": "test-model", "messages": [{"role": "user", "content": "Hello"}]}
            request("POST", "/v1/chat/completions", chat, token=key["key"])
            request("POST", "/v1/chat/completions", chat, token=key["key"])
            error = request("POST", "/v1/chat/completions", chat, token=key["key"], expected=403)
            assert error["error"]["code"] == "token_budget_exhausted"
            print("Soft token threshold and model filtering passed.", flush=True)
            docker("restart", "-t", "5", name)
            base = base_url()
            ready()
            status = request("GET", "/api/v1/key", token=key["key"])
            assert status["requests_used"] == 2 and status["tokens_used"] == 150
            print("Usage survived container restart.", flush=True)
            key2 = issue(1, 1_000_000)
            request("POST", "/v1/chat/completions", {**chat, "stream": True}, token=key2["key"])
            error = request("POST", "/v1/chat/completions", chat, token=key2["key"], expected=403)
            assert error["error"]["code"] == "request_limit_reached"
            key3 = issue()
            request("DELETE", path + "/keys/" + key3["id"], admin=True, expected=204)
            error = request("POST", "/v1/chat/completions", chat, token=key3["key"], expected=403)
            assert error["error"]["code"] == "key_revoked"
            request("DELETE", path, admin=True, expected=204)
            request("GET", "/api/v1/key", token=key["key"], expected=401)
            docker("exec", name, "/tabsgate", "healthcheck")
            print("SSE, request cap, revocation, deletion and healthcheck passed.", flush=True)
    except BaseException:
        if created:
            print(docker("logs", name))
        raise
    finally:
        if created:
            subprocess.run(["docker", "rm", "-f", name], check=False, stdout=subprocess.DEVNULL)
        subprocess.run(["docker", "volume", "rm", volume], check=False, stdout=subprocess.DEVNULL)
        provider.shutdown()
        provider.server_close()


if __name__ == "__main__":
    main()
