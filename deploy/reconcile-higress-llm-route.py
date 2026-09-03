#!/usr/bin/env python3
import argparse
import http.cookiejar
import json
import os
import pathlib
import urllib.error
import urllib.request


DEFAULT_MANIFEST = pathlib.Path(__file__).with_name("higress-llm-route.desired.json")


class ConsoleClient:
    def __init__(self, base_url, username, password):
        self.base_url = base_url.rstrip("/")
        self.cookies = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.cookies))
        self.login(username, password)

    def login(self, username, password):
        request = urllib.request.Request(
            self.base_url + "/session/login",
            data=json.dumps({"username": username, "password": password}).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with self.opener.open(request) as response:
            if response.status not in (200, 201):
                raise RuntimeError(f"Higress login failed with HTTP {response.status}")

    def request(self, method, path, payload=None, allow_not_found=False):
        data = None if payload is None else json.dumps(payload).encode()
        request = urllib.request.Request(
            self.base_url + path,
            data=data,
            headers={"Content-Type": "application/json"},
            method=method,
        )
        try:
            with self.opener.open(request) as response:
                body = response.read()
                return response.status, json.loads(body) if body else {}
        except urllib.error.HTTPError as error:
            if allow_not_found and error.code == 404:
                return 404, {}
            raise RuntimeError(f"Higress {method} {path} failed with HTTP {error.code}") from error


def load_desired(manifest_path, api_key):
    desired = json.loads(manifest_path.read_text(encoding="utf-8"))
    provider = desired["provider"]
    provider["tokens"] = [api_key]
    provider["rawConfigs"]["apiTokens"] = [api_key]
    return desired


def merge(current, desired):
    merged = dict(current)
    merged.update(desired)
    return merged


def reconcile(client, desired, check_only):
    provider_name = desired["provider"]["name"]
    route_name = desired["route"]["name"]
    provider_status, provider_response = client.request(
        "GET", f"/v1/ai/providers/{provider_name}", allow_not_found=True
    )
    route_status, route_response = client.request(
        "GET", f"/v1/ai/routes/{route_name}", allow_not_found=True
    )
    current_provider = provider_response.get("data", {})
    current_route = route_response.get("data", {})

    provider_valid = (
        provider_status == 200
        and current_provider.get("name") == provider_name
        and bool(current_provider.get("tokens"))
        and current_provider.get("rawConfigs", {}).get("openaiCustomUrl")
        == desired["provider"]["rawConfigs"]["openaiCustomUrl"]
    )
    route_valid = (
        route_status == 200
        and current_route.get("upstreams") == desired["route"]["upstreams"]
        and current_route.get("pathPredicate", {}).get("matchValue") == "/v1"
        and current_route.get("authConfig", {}).get("enabled") is True
    )
    if check_only:
        if not provider_valid or not route_valid:
            raise SystemExit("Higress LLM route does not match the declarative manifest")
        print(
            "Higress LLM route matches the declarative manifest: "
            f"provider={provider_name}, route={route_name}, version={current_route.get('version')}"
        )
        return

    if provider_status == 404:
        status, _ = client.request("POST", "/v1/ai/providers", desired["provider"])
    elif not provider_valid:
        status, _ = client.request(
            "PUT", f"/v1/ai/providers/{provider_name}", merge(current_provider, desired["provider"])
        )
    else:
        status = 200

    if route_status == 404:
        route_payload = desired["route"]
        route_payload.setdefault("authConfig", {}).setdefault("allowedConsumers", ["manager"])
        route_status, _ = client.request("POST", "/v1/ai/routes", route_payload)
    elif not route_valid:
        route_payload = merge(current_route, desired["route"])
        route_status, _ = client.request("PUT", f"/v1/ai/routes/{route_name}", route_payload)
    else:
        route_status = 200

    _, refreshed = client.request("GET", f"/v1/ai/routes/{route_name}")
    refreshed_route = refreshed.get("data", {})
    if refreshed_route.get("upstreams") != desired["route"]["upstreams"]:
        raise SystemExit("Higress route reconciliation verification failed")
    print(
        "Higress LLM route reconciled: "
        f"provider={provider_name}, route={route_name}, version={refreshed_route.get('version')}"
    )


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--manifest", type=pathlib.Path, default=DEFAULT_MANIFEST)
    parser.add_argument(
        "--console-url",
        default=os.environ.get("HIGRESS_CONSOLE_URL", "http://127.0.0.1:8001"),
    )
    parser.add_argument("--check", action="store_true")
    arguments = parser.parse_args()
    username = os.environ.get("HIGRESS_ADMIN_USER")
    password = os.environ.get("HIGRESS_ADMIN_PASSWORD")
    if not username or not password:
        raise SystemExit("HIGRESS_ADMIN_USER and HIGRESS_ADMIN_PASSWORD are required")
    if arguments.check:
        api_key = "<check-only-non-empty-placeholder>"
    else:
        api_key = os.environ.get("MINIMAX_API_KEY")
        if not api_key:
            raise SystemExit("MINIMAX_API_KEY is required unless --check is used")
    client = ConsoleClient(arguments.console_url, username, password)
    reconcile(client, load_desired(arguments.manifest, api_key), arguments.check)


if __name__ == "__main__":
    main()
