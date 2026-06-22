#!/usr/bin/env python3
"""Regression test for Step-2 "switch to password" handling.

Microsoft now interrupts the email->password flow with an Authenticator-first
screen: either a "Choose a way to sign in" picker, or a direct "Approve sign in"
push page. The password field only appears after clicking "Use my password" /
"Use your password instead". switch_to_password() must click that control on
BOTH pages, and must no-op (return None) when the password field is already
showing (the old direct-to-password flow).

Fixtures under tests/fixtures/ are real rendered-DOM dumps captured on
2026-06-19 when the VPN auto-login started failing at Step 2.

Run: .venv/bin/python tests/test_switch_to_password.py
Exits non-zero on failure. No pytest dependency.
"""
import importlib.util
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
FIXTURES = Path(__file__).resolve().parent / "fixtures"

# hkust-vpn.py has a hyphen in its name -> load it by path.
spec = importlib.util.spec_from_file_location("hkust_vpn", ROOT / "hkust-vpn.py")
hkust_vpn = importlib.util.module_from_spec(spec)
spec.loader.exec_module(hkust_vpn)

from playwright.sync_api import sync_playwright


def _load_fixture(page, name):
    page.goto((FIXTURES / name).as_uri(), wait_until="domcontentloaded", timeout=15000)


def main():
    failures = []
    with sync_playwright() as p:
        browser = p.firefox.launch(headless=True)
        ctx = browser.new_context()
        # Block all network so the saved page's external Microsoft JS bundle
        # cannot run, redirect, or rewrite the DOM. We test the static rendered
        # DOM exactly as it looked when login hung.
        ctx.route(re.compile(r"^https?://"), lambda route: route.abort())
        page = ctx.new_page()

        # 1) "Choose a way to sign in" picker -> click the password tile.
        _load_fixture(page, "ms-choose-method.html")
        r1 = hkust_vpn.switch_to_password(page)
        if not r1:
            failures.append(f"choose-method: expected a click tag, got {r1!r}")
        else:
            print(f"[ok] choose-method -> {r1!r}")

        # 2) "Approve sign in" push page -> click "Use your password instead".
        _load_fixture(page, "ms-push-approve.html")
        r2 = hkust_vpn.switch_to_password(page)
        if r2 != "switch_link":
            failures.append(f"push-approve: expected 'switch_link', got {r2!r}")
        else:
            print(f"[ok] push-approve -> {r2!r}")

        # 3) Password field already present -> must NOT click anything.
        page.set_content(
            '<html><body><form>'
            '<input name="passwd" type="password"></form></body></html>',
            wait_until="domcontentloaded",
        )
        r3 = hkust_vpn.switch_to_password(page)
        if r3 is not None:
            failures.append(f"passwd-present: expected None, got {r3!r}")
        else:
            print(f"[ok] passwd-present -> {r3!r}")

        browser.close()

    if failures:
        print("\nFAIL:")
        for f in failures:
            print("  -", f)
        sys.exit(1)
    print("\nALL PASS")


if __name__ == "__main__":
    main()
