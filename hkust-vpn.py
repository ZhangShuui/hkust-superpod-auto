#!/usr/bin/env python3
"""
HKUST VPN Connect Script
Fully automated: browser login + TOTP MFA + openconnect split tunneling.
Zero interaction required after initial credential setup.
"""

import subprocess
import sys
import os
import argparse
import getpass
import json
import time
from pathlib import Path

import pyotp
from playwright.sync_api import sync_playwright


VPN_URL = "https://remote.ust.hk/mfa"
DEFAULT_USER = "szhangfa@connect.ust.hk"
DEFAULT_PROXY = "http://127.0.0.1:7890"
DEFAULT_HOSTS = ["superpod.ust.hk"]
VPN_SLICE_PATH = "/home/shurui/anaconda3/bin/vpn-slice"
CRED_FILE = Path.home() / ".config" / "hkust-vpn" / "credentials.json"


# ─── Credential Management ────────────────────────────────────────────

def save_credentials(user, password, totp_secret, sudo_password):
    """Save credentials to local file (chmod 600)."""
    CRED_FILE.parent.mkdir(parents=True, exist_ok=True)
    CRED_FILE.write_text(json.dumps({
        "user": user,
        "password": password,
        "totp_secret": totp_secret,
        "sudo_password": sudo_password,
    }))
    os.chmod(CRED_FILE, 0o600)
    print(f"[+] Credentials saved to {CRED_FILE}")


def load_credentials():
    """Load saved credentials."""
    if CRED_FILE.exists():
        return json.loads(CRED_FILE.read_text())
    return {}


def setup_credentials(user):
    """Interactive credential setup."""
    print("[*] First-time setup: enter your credentials.")
    password = getpass.getpass(f"[?] HKUST password for {user}: ")
    totp_secret = input("[?] TOTP secret key (Base32): ").strip()
    sudo_password = getpass.getpass("[?] sudo password (local machine): ")

    # Verify TOTP works
    try:
        totp = pyotp.TOTP(totp_secret)
        code = totp.now()
        print(f"[+] TOTP test: current code = {code} (looks good)")
    except Exception as e:
        print(f"[!] TOTP secret seems invalid: {e}")
        sys.exit(1)

    save_credentials(user, password, totp_secret, sudo_password)
    return password, totp_secret, sudo_password


# ─── Browser Login Automation ─────────────────────────────────────────

def get_dsid_cookie(user, password, totp_secret, proxy=None, headless=False):
    """Fully automated: open browser, login, MFA via TOTP, return DSID cookie."""
    totp = pyotp.TOTP(totp_secret)

    print("[*] Starting automated VPN login...")

    with sync_playwright() as p:
        launch_opts = {
            "headless": headless,
            "args": [
                "--disable-blink-features=AutomationControlled",
                "--no-sandbox",
            ],
        }
        if proxy:
            launch_opts["proxy"] = {"server": proxy}
            launch_opts["args"].append(f"--proxy-server={proxy}")

        browser = p.chromium.launch(**launch_opts)
        context = browser.new_context(
            user_agent="Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
                       "AppleWebKit/537.36 (KHTML, like Gecko) "
                       "Chrome/120.0.0.0 Safari/537.36",
            ignore_https_errors=True,
        )
        # Disable WebAuthn/FIDO2 so the browser never pops up the
        # "Use your security key" native dialog
        context.add_init_script("""
            delete window.PublicKeyCredential;
            if (navigator.credentials) {
                navigator.credentials.get = () => Promise.reject(new Error('disabled'));
                navigator.credentials.create = () => Promise.reject(new Error('disabled'));
            }
        """)
        page = context.new_page()

        try:
            page.goto(VPN_URL, wait_until="domcontentloaded", timeout=60000)

            # ── Step 1: Enter email ──
            print("[*] Step 1/5: Entering email...")
            page.wait_for_selector('input[name="loginfmt"]', timeout=30000)
            page.fill('input[name="loginfmt"]', user)
            page.wait_for_timeout(300)
            page.click("#idSIButton9")
            page.wait_for_load_state("networkidle", timeout=15000)
            print(f"[+] Email: {user}")

            # ── Step 2: Enter password ──
            print("[*] Step 2/5: Entering password...")
            page.wait_for_selector('input[name="passwd"]', state="visible", timeout=15000)
            page.wait_for_timeout(500)
            page.fill('input[name="passwd"]', password)
            page.wait_for_timeout(300)
            page.click("#idSIButton9")
            page.wait_for_load_state("networkidle", timeout=15000)
            print("[+] Password entered.")

            # ── Step 3: On "Verify your identity" → click "Use a verification code" ──
            print("[*] Step 3/5: Switching to TOTP...")
            page.wait_for_timeout(3000)

            # If we still land on FIDO2 page somehow, click Back first
            try:
                back = page.locator("#idBtn_Back")
                if back.is_visible(timeout=2000):
                    print("[*]  -> Clicking 'Back' to exit FIDO2...")
                    back.click()
                    page.wait_for_timeout(3000)
            except Exception:
                pass

            # Click "Use a verification code"
            print("[*]  -> Looking for 'Use a verification code'...")
            page.evaluate("""() => {
                const els = document.querySelectorAll('div, a, button, li, span, p');
                for (const el of els) {
                    const direct = [...el.childNodes]
                        .filter(n => n.nodeType === 3)
                        .map(n => n.textContent.trim())
                        .join(' ');
                    if (direct.includes('Use a verification code')) {
                        // Click the element or its closest clickable parent
                        const clickable = el.closest('[data-value], [role=button], a, button') || el;
                        clickable.click();
                        return;
                    }
                }
                // Fallback: broader match
                for (const el of els) {
                    if (el.textContent?.includes('verification code') &&
                        !el.textContent?.includes('Approve') &&
                        el.offsetParent !== null &&
                        el.children.length < 5) {
                        el.click();
                        return;
                    }
                }
            }""")
            page.wait_for_timeout(3000)
            print("[+] Clicked 'Use a verification code'.")

            # ── Step 4: Enter TOTP code ──
            print("[*] Step 4/5: Entering TOTP code...")
            code = totp.now()
            otp_input = page.wait_for_selector(
                "#idTxtBx_SAOTCC_OTC, input[name='otc'], input[type='tel'], input[type='number']",
                timeout=10000,
            )
            otp_input.fill(code)
            page.wait_for_timeout(300)
            page.click("#idSubmit_SAOTCC_Continue")
            page.wait_for_load_state("networkidle", timeout=15000)
            print(f"[+] TOTP code: {code}")

            # ── Step 5: "Stay signed in?" → Yes ──
            print("[*] Step 5/5: Stay signed in...")
            try:
                page.wait_for_selector("#idSIButton9", timeout=10000)
                page.click("#idSIButton9")
                print("[+] Clicked 'Yes'.")
            except Exception:
                pass

            # ── Wait for DSID cookie ──
            print("[*] Waiting for VPN session cookie...")
            dsid = None
            for _ in range(120):
                page.wait_for_timeout(1000)
                cookies = context.cookies("https://remote.ust.hk")
                for cookie in cookies:
                    if cookie["name"] == "DSID":
                        dsid = cookie["value"]
                        break
                if dsid:
                    break

        except Exception as e:
            print(f"[!] Automated login error: {e}")
            print("[*] Falling back to manual login. Complete in the browser.")
            dsid = None
            for _ in range(300):
                try:
                    page.wait_for_timeout(1000)
                    cookies = context.cookies("https://remote.ust.hk")
                    for cookie in cookies:
                        if cookie["name"] == "DSID":
                            dsid = cookie["value"]
                            break
                    if dsid:
                        break
                except Exception:
                    break
        finally:
            browser.close()

    if dsid:
        print(f"[+] Got DSID cookie: {dsid[:20]}...")
        return dsid
    else:
        print("[-] Failed to get DSID cookie.")
        sys.exit(1)


# ─── VPN Connection ───────────────────────────────────────────────────

def connect_vpn(dsid, proxy=None, hosts=None, sudo_password=None):
    """Launch openconnect with the DSID cookie and vpn-slice."""
    hosts = hosts or DEFAULT_HOSTS
    vpn_slice_arg = VPN_SLICE_PATH + " " + " ".join(hosts)

    cmd = [
        "sudo", "-S",  # read password from stdin
        "openconnect",
        "--protocol=nc",
        VPN_URL,
        f"--cookie=DSID={dsid}",
        "-s", vpn_slice_arg,
    ]

    if proxy:
        cmd.extend(["--proxy", proxy])

    print(f"[*] Connecting VPN (split tunnel: {', '.join(hosts)})")

    try:
        proc = subprocess.Popen(
            cmd,
            stdin=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        # Send sudo password
        if sudo_password:
            proc.stdin.write((sudo_password + "\n").encode())
            proc.stdin.flush()
        proc.wait()
    except KeyboardInterrupt:
        print("\n[*] VPN disconnected.")
        raise  # re-raise so auto_reconnect knows it was user-initiated


def auto_reconnect(user, password, totp_secret, sudo_password,
                   proxy=None, hosts=None, headless=False):
    """Auto-reconnect loop: re-login and reconnect when session expires.

    HKUST VPN limits:
      - Inactivity timeout: 30 min
      - Max session length: 240 min (4 hours)
    """
    attempt = 0
    while True:
        attempt += 1
        start = time.time()
        print(f"\n{'='*50}")
        print(f"[*] VPN session #{attempt} starting at {time.strftime('%H:%M:%S')}")
        print(f"[*] Session will expire in ~4 hours (240 min)")
        print(f"{'='*50}\n")

        try:
            dsid = get_dsid_cookie(user, password, totp_secret,
                                   proxy=proxy, headless=headless)
            connect_vpn(dsid, proxy=proxy, hosts=hosts,
                        sudo_password=sudo_password)
        except KeyboardInterrupt:
            print("\n[*] User interrupted. Exiting.")
            break

        elapsed = time.time() - start
        print(f"\n[!] VPN disconnected after {elapsed/60:.1f} minutes.")

        if elapsed < 30:
            # If it disconnected very quickly, something is wrong
            wait = 10
            print(f"[!] Session too short. Waiting {wait}s before retry...")
        else:
            wait = 5
            print(f"[*] Reconnecting in {wait}s... (Ctrl+C to stop)")

        try:
            time.sleep(wait)
        except KeyboardInterrupt:
            print("\n[*] User interrupted. Exiting.")
            break


# ─── Main ─────────────────────────────────────────────────────────────

def main():
    parser = argparse.ArgumentParser(
        description="HKUST VPN - fully automated split tunneling",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""Examples:
  %(prog)s                             # Connect with auto-reconnect (default)
  %(prog)s --headless                  # No browser window
  %(prog)s --no-reconnect              # Single session, no auto-reconnect
  %(prog)s --hosts a.ust.hk b.ust.hk  # Multiple hosts through VPN
  %(prog)s --setup                     # Re-enter credentials
  %(prog)s --cookie DSID_VALUE         # Skip login, use cookie directly
""",
    )
    parser.add_argument("-u", "--user", default=DEFAULT_USER)
    parser.add_argument("--proxy", default=DEFAULT_PROXY)
    parser.add_argument("--no-proxy", action="store_true")
    parser.add_argument("--hosts", nargs="+", default=DEFAULT_HOSTS)
    parser.add_argument("--cookie", help="Skip login, use DSID cookie directly")
    parser.add_argument("--setup", action="store_true", help="Re-enter credentials")
    parser.add_argument("--headless", action="store_true", help="No browser window")
    parser.add_argument("--no-reconnect", action="store_true",
                        help="Disable auto-reconnect (single session only)")

    args = parser.parse_args()
    proxy = None if args.no_proxy else args.proxy

    # Load or setup credentials
    creds = load_credentials()
    if args.setup or not creds.get("password") or not creds.get("totp_secret"):
        password, totp_secret, sudo_password = setup_credentials(args.user)
    else:
        password = creds["password"]
        totp_secret = creds["totp_secret"]
        sudo_password = creds.get("sudo_password", "")
        args.user = creds.get("user", args.user)
        if not sudo_password:
            sudo_password = getpass.getpass("[?] sudo password: ")

    if args.cookie:
        dsid = args.cookie
        connect_vpn(dsid, proxy=proxy, hosts=args.hosts, sudo_password=sudo_password)
    elif args.no_reconnect:
        dsid = get_dsid_cookie(
            args.user, password, totp_secret,
            proxy=proxy, headless=args.headless,
        )
        connect_vpn(dsid, proxy=proxy, hosts=args.hosts, sudo_password=sudo_password)
    else:
        auto_reconnect(
            args.user, password, totp_secret, sudo_password,
            proxy=proxy, hosts=args.hosts, headless=args.headless,
        )


if __name__ == "__main__":
    main()
