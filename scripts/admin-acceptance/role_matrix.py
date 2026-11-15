#!/usr/bin/env python3
"""Exercise real local role-bound BFF and Go API; logs never contain secrets.

Uses uniquely named currency canaries (then disables them) and pending deposit
bookings for dedicated holder 990051. Never settles, credits or changes an
existing policy/period. Optional add-ons are expected to remain disabled.
"""
import argparse
import base64
import hashlib
import hmac
import json
import secrets
import stat
import sys
import time
from http.cookies import SimpleCookie
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", required=True)
    parser.add_argument("--api-url", required=True)
    parser.add_argument("--env-file", required=True)
    parser.add_argument("--fixture-holder", type=int, default=990051, help="dedicated positive holder for pending-only permission fixtures")
    args = parser.parse_args()
    if args.fixture_holder <= 0 or args.fixture_holder > 9007199254740991:
        raise ValueError("fixture-holder must be a positive safe integer")
    base, api = args.base_url.rstrip("/"), args.api_url.rstrip("/")
    for endpoint in (base, api):
        parsed = urlsplit(endpoint)
        if parsed.hostname != "127.0.0.1" or parsed.path or parsed.query or parsed.fragment or parsed.username:
            raise ValueError("only isolated loopback test hosts are supported")
    env_path = Path(args.env_file)
    if stat.S_IMODE(env_path.stat().st_mode) & 0o077:
        raise ValueError("credential JSON must have mode 0600 or stricter")
    env = json.loads(env_path.read_text())
    opener = build_opener(NoRedirect())
    outcomes = []
    tokens = {}
    prefix = "AUTHROLE_" + secrets.token_hex(5).upper()

    def request(target, method, path, body=None, role=None, origin=None, cookie=None, headers=None):
        actual_headers = dict(headers or {})
        if role is not None:
            if target == base:
                actual_headers["Cookie"] = "ledger_acceptance_session=" + tokens[role]
            else:
                actual_headers["Authorization"] = "Bearer " + env["LEDGER_KEY_" + role.upper()]
        if cookie:
            actual_headers["Cookie"] = "ledger_acceptance_session=" + cookie
        if origin is not None:
            actual_headers["Origin"] = origin
        if body is not None:
            actual_headers.setdefault("Content-Type", "application/json")
        req = Request(target + path, data=None if body is None else json.dumps(body).encode(), headers=actual_headers, method=method)
        try:
            response = opener.open(req, timeout=15)
        except HTTPError as error:
            response = error
        with response:
            raw = response.read(2 * 1024 * 1024)
            try:
                data = json.loads(raw)
            except (ValueError, UnicodeDecodeError):
                data = None
            return response.status, response.headers, data

    def record(name, result, expected, extra=None):
        row = {"case": name, "status": result[0], "expected": expected, "pass": result[0] in expected}
        if isinstance(result[2], dict) and isinstance(result[2].get("code"), int):
            row["code"] = result[2]["code"]
        if extra:
            row.update(extra)
        outcomes.append(row)
        print(json.dumps(row), flush=True)

    def value_check(name, passed, **extra):
        row = {"case": name, "pass": bool(passed), **extra}
        outcomes.append(row)
        print(json.dumps(row), flush=True)

    def list_currencies(role="reader"):
        result = request(base, "GET", "/api/v1/currencies?active_only=false", role=role)
        if result[0] != 200 or not isinstance(result[2], dict) or not isinstance(result[2].get("data", {}).get("list"), list):
            raise RuntimeError("currency query failed")
        return result[2]["data"]["list"]

    for path in ("/api/v1/currencies", "/api/v1/journals"):
        record("anonymous_bff_" + path.rsplit("/", 1)[1], request(base, "GET", path), [401])
        record("anonymous_go_" + path.rsplit("/", 1)[1], request(api, "GET", path), [401])
    record("go_public_health", request(api, "GET", "/api/v1/system/health"), [200])
    record("go_invalid_key", request(api, "GET", "/api/v1/currencies", headers={"Authorization": "Bearer invalid-probe-key"}), [401])
    record("login_wrong_password", request(base, "POST", "/api/session", {"account": "reader", "password": secrets.token_hex(16)}, origin=base), [401])
    record("login_wrong_account_password_pair", request(base, "POST", "/api/session", {"account": "admin", "password": env["ADMIN_PASSWORD_READER"]}, origin=base), [401])
    record("login_non_string_password", request(base, "POST", "/api/session", {"account": "admin", "password": 42}, origin=base), [400, 401])
    for label, origin in (("missing", None), ("foreign", "https://rejected-origin.invalid"), ("null", "null")):
        record("login_" + label + "_origin", request(base, "POST", "/api/session", {"account": "admin", "password": env["ADMIN_PASSWORD_ADMIN"]}, origin=origin), [403])

    for role in ("reader", "writer", "admin", "reviewer"):
        result = request(base, "POST", "/api/session", {"account": role, "password": env["ADMIN_PASSWORD_" + role.upper()]}, origin=base)
        record(role + "_login", result, [200])
        jar = SimpleCookie()
        for header in result[1].get_all("Set-Cookie", []):
            jar.load(header)
        if result[0] != 200 or "ledger_acceptance_session" not in jar:
            raise RuntimeError("role login failed")
        morsel = jar["ledger_acceptance_session"]
        tokens[role] = morsel.value
        value_check(role + "_cookie_flags", bool(morsel["httponly"]) and morsel["samesite"].lower() == "strict", secure=bool(morsel["secure"]), loopback_http_exception=True)
        record(role + "_bff_read", request(base, "GET", "/api/v1/currencies", role=role), [200])
        record(role + "_go_read", request(api, "GET", "/api/v1/currencies", role=role), [200])

    usd = next(row["uid"] for row in list_currencies() if row["code"] == "USD")
    for role in ("reader", "writer", "admin", "reviewer"):
        for label, target in (("bff", base), ("go", api)):
            record(role + "_" + label + "_batch_read", request(target, "POST", "/api/v1/balances/batch", {"holder_ids": [1001], "currency_uid": usd}, role=role, origin=base), [200])
            record(role + "_" + label + "_preview", request(target, "POST", "/api/v1/templates/deposit_confirm/preview", {"holder_id": 1001, "currency_uid": usd, "amounts": {"amount": "0.01"}}, role=role, origin=base), [200])
            code = prefix + "_" + role.upper() + "_" + label.upper()
            result = request(target, "POST", "/api/v1/currencies", {"code": code, "name": "Role boundary canary", "exponent": 2}, role=role, origin=base)
            record(role + "_" + label + "_admin_create", result, [201] if role == "admin" else [403], {"canary_code": code})
            rows = [row for row in list_currencies() if row["code"] == code]
            value_check(role + "_" + label + "_config_storage", bool(rows) == (role == "admin"), persisted=bool(rows))
            for row in rows:
                record("cleanup_" + code, request(base, "POST", "/api/v1/currencies/" + row["uid"] + "/deactivate", {}, role="admin", origin=base), [200])
            booking = {"classification_code": "deposit", "account_holder": args.fixture_holder, "currency_uid": usd, "amount": "0.01", "idempotency_key": prefix + ":booking:" + role, "metadata": {"purpose": "permission probe; pending, never credited"}}
            record(role + "_" + label + "_business_write", request(target, "POST", "/api/v1/bookings", booking, role=role, origin=base), [201] if role in ("writer", "admin") else [403])
            for suffix, body in (("approve", {}), ("reject", {"reason": "isolated capability probe"})):
                record(role + "_" + label + "_review_" + suffix, request(target, "POST", "/api/v1/deposits/auth-probe-missing/review/" + suffix, body, role=role, origin=base), [503] if role == "reviewer" else [403])
            record(role + "_" + label + "_dead_letter_replay", request(target, "POST", "/api/v1/deposits/dead-letters/auth-probe-missing/replay", {}, role=role, origin=base), [503] if role == "reviewer" else [403])
            record(role + "_" + label + "_protected_template", request(target, "POST", "/api/v1/journals/template", {"template_code": "deposit_confirm", "holder_id": 990051, "currency_uid": usd, "amounts": {"amount": "0.01"}, "idempotency_key": prefix + ":protected:" + role + ":" + label}, role=role, origin=base), [403])

    forged = {"Authorization": "Bearer " + env["LEDGER_KEY_ADMIN"], "X-Role": "admin", "X-Account": "admin"}
    # Credentials here stay in this server-side test process, never the browser.
    code = prefix + "_FORGED"
    record("reader_cannot_select_admin_key_or_role", request(base, "POST", "/api/v1/currencies?role=admin", {"code": code, "name": "Forged role canary", "exponent": 2, "role": "admin"}, role="reader", origin=base, headers=forged), [403])
    value_check("forged_role_no_storage", not any(row["code"] == code for row in list_currencies()))
    for label, origin in (("missing", None), ("foreign", "https://rejected-origin.invalid"), ("null", "null")):
        code = prefix + "_ORIGIN_" + label.upper()
        record("admin_write_" + label + "_origin", request(base, "POST", "/api/v1/currencies", {"code": code, "name": "Rejected Origin canary", "exponent": 2}, role="admin", origin=origin, headers={"Content-Type": "text/plain"}), [403])
        value_check("origin_" + label + "_no_storage", not any(row["code"] == code for row in list_currencies()))
        record("logout_" + label + "_origin", request(base, "DELETE", "/api/session", role="admin", origin=origin), [403])
    original = tokens["admin"]
    tampered = original[:-1] + ("0" if original[-1] != "0" else "1")
    record("tampered_session", request(base, "GET", "/api/v1/currencies", cookie=tampered), [401])
    payload = base64.urlsafe_b64encode(json.dumps({"account": "admin", "nonce": "00000000-0000-0000-0000-000000000000", "expiresAt": int(time.time() * 1000) - 60_000}, separators=(",", ":")).encode()).decode().rstrip("=")
    expired = payload + "." + hmac.new(env["ADMIN_SESSION_SECRET"].encode(), payload.encode(), hashlib.sha256).hexdigest()
    record("expired_session", request(base, "GET", "/api/v1/currencies", cookie=expired), [401])
    for role in tokens:
        record(role + "_logout", request(base, "DELETE", "/api/session", role=role, origin=base), [200])
    failures = sum(not item["pass"] for item in outcomes)
    print(json.dumps({"summary": "actual BFF and Go API; no browser RBAC assumptions", "checks": len(outcomes), "failed": failures, "fixture_holder": args.fixture_holder, "fixture_prefix": prefix}), flush=True)
    return 1 if failures else 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as error:
        print(json.dumps({"probe_error_type": type(error).__name__, "detail": "probe failed without logging credentials"}), flush=True)
        sys.exit(2)
