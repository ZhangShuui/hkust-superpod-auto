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
import random
import logging
from logging.handlers import RotatingFileHandler
from pathlib import Path

import socket
import struct
import threading

import pyotp
from playwright.sync_api import sync_playwright


def _load_dotenv():
    """Load .env file from project root if it exists."""
    env_file = Path(__file__).resolve().parent / ".env"
    if not env_file.exists():
        return
    for line in env_file.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        key, _, value = line.partition("=")
        key, value = key.strip(), value.strip()
        # Strip inline comments (outside quotes)
        if not (len(value) >= 2 and value[0] in ('"', "'") and value[0] == value[-1]):
            if "#" in value:
                value = value.split("#")[0].rstrip()
        # Strip matching quotes
        if len(value) >= 2 and value[0] == value[-1] and value[0] in ('"', "'"):
            value = value[1:-1]
        if key and key not in os.environ:  # don't override existing env
            os.environ[key] = value

_load_dotenv()

VPN_URL = "https://remote.ust.hk/mfa"
DEFAULT_USER = os.environ.get("HKUST_USER", "")
_clash_port = os.environ.get("CLASH_PORT", "7897")
DEFAULT_PROXY = os.environ.get("VPN_PROXY", f"http://127.0.0.1:{_clash_port}")
_vpn_hosts = os.environ.get("VPN_HOSTS", "")
DEFAULT_HOSTS = _vpn_hosts.split() if _vpn_hosts else [os.environ.get("SUPERPOD_HOST", "superpod.ust.hk")]
VPN_SLICE_PATH = os.environ.get(
    "VPN_SLICE_PATH",
    str(Path(__file__).resolve().parent / ".venv" / "bin" / "vpn-slice"),
)
CRED_FILE = Path.home() / ".config" / "hkust-vpn" / "credentials.json"

# ─── Retry / Timeout Constants ────────────────────────────────────────
LOGIN_TIMEOUT = 180          # 3 min overall login timeout
MAX_CONSECUTIVE_FAILURES = 5
BACKOFF_BASE = 10            # seconds
BACKOFF_CAP = 300            # 5 minutes
HEALTH_CHECK_INTERVAL = 60   # seconds

# ─── Logging ──────────────────────────────────────────────────────────

LOG_FILE = Path(__file__).resolve().parent / "vpn.log"

def _setup_logging():
    """Dual logging: console (if tty) + rotating file (5 MB, 3 backups)."""
    root = logging.getLogger()
    root.setLevel(logging.INFO)
    # File handler
    fh = RotatingFileHandler(LOG_FILE, maxBytes=5*1024*1024, backupCount=3)
    fh.setFormatter(logging.Formatter("%(asctime)s %(message)s", datefmt="%Y-%m-%d %H:%M:%S"))
    root.addHandler(fh)
    # Console handler — only when stdout is a terminal (avoids double-logging in background)
    if sys.stdout.isatty():
        ch = logging.StreamHandler()
        ch.setFormatter(logging.Formatter("%(message)s"))
        root.addHandler(ch)

_setup_logging()
log = logging.getLogger("hkust-vpn")


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
    log.info(f"[+] Credentials saved to {CRED_FILE}")


def load_credentials():
    """Load credentials from env vars first, then fall back to credentials file."""
    creds = {}
    if CRED_FILE.exists():
        creds = json.loads(CRED_FILE.read_text())
    # Env vars take priority
    env_map = {
        "user": "HKUST_USER",
        "password": "HKUST_PASSWORD",
        "totp_secret": "HKUST_TOTP_SECRET",
        "sudo_password": "SUDO_PASSWORD",
    }
    for key, env_var in env_map.items():
        val = os.environ.get(env_var, "")
        if val:
            creds[key] = val
    return creds


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
        log.info(f"[+] TOTP test: current code = {code} (looks good)")
    except Exception as e:
        log.error(f"[!] TOTP secret seems invalid: {e}")
        sys.exit(1)

    save_credentials(user, password, totp_secret, sudo_password)
    return password, totp_secret, sudo_password


# ─── Browser Login Automation ─────────────────────────────────────────

def get_dsid_cookie(user, password, totp_secret, proxy=None, headless=False):
    """Fully automated: open browser, login, MFA via TOTP, return DSID cookie.

    Returns the DSID cookie string on success, or None on failure.
    Has an overall timeout of LOGIN_TIMEOUT seconds.
    """
    totp = pyotp.TOTP(totp_secret)
    deadline = time.monotonic() + LOGIN_TIMEOUT

    log.info("[*] Starting automated VPN login...")

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
            log.info("[*] Step 1/5: Entering email...")
            page.wait_for_selector('input[name="loginfmt"]', timeout=30000)
            page.fill('input[name="loginfmt"]', user)
            page.wait_for_timeout(300)
            page.click("#idSIButton9")
            page.wait_for_load_state("networkidle", timeout=15000)
            log.info(f"[+] Email: {user}")

            # ── Step 2: Enter password ──
            log.info("[*] Step 2/5: Entering password...")
            page.wait_for_selector('input[name="passwd"]', state="visible", timeout=15000)
            page.wait_for_timeout(500)
            page.fill('input[name="passwd"]', password)
            page.wait_for_timeout(300)
            page.click("#idSIButton9")
            page.wait_for_load_state("networkidle", timeout=15000)
            log.info("[+] Password entered.")

            # ── Step 3: On "Verify your identity" → click "Use a verification code" ──
            log.info("[*] Step 3/5: Switching to TOTP...")
            page.wait_for_timeout(5000)

            # Debug: screenshot + dump visible text for MFA page analysis
            debug_dir = Path(__file__).resolve().parent
            try:
                page.screenshot(path=str(debug_dir / "mfa-debug.png"))
                page_text = page.evaluate("() => document.body.innerText")
                (debug_dir / "mfa-debug.txt").write_text(page_text)
                log.info(f"[*] MFA page debug saved to mfa-debug.png/txt")
            except Exception as e:
                log.warning(f"[!] Debug screenshot failed: {e}")

            # Try to find "Use a verification code" — with retries and Back fallback
            found = 'not_found'
            for attempt in range(3):
                found = page.evaluate("""() => {
                    const els = document.querySelectorAll('div, a, button, li, span, p');
                    for (const el of els) {
                        const direct = [...el.childNodes]
                            .filter(n => n.nodeType === 3)
                            .map(n => n.textContent.trim())
                            .join(' ');
                        if (direct.includes('Use a verification code')) {
                            const clickable = el.closest('[data-value], [role=button], a, button') || el;
                            clickable.click();
                            return 'found_direct';
                        }
                    }
                    for (const el of els) {
                        if (el.textContent?.includes('verification code') &&
                            !el.textContent?.includes('Approve') &&
                            el.offsetParent !== null &&
                            el.children.length < 5) {
                            el.click();
                            return 'found_fallback';
                        }
                    }
                    return 'not_found';
                }""")
                log.info(f"[*]  -> Attempt {attempt+1}: click result = {found}")
                if found != 'not_found':
                    break

                # Not found — try clicking "I can't use my Microsoft Authenticator app right now"
                alt = page.evaluate("""() => {
                    const els = document.querySelectorAll('a[id], a[href], button, div[role=button]');
                    for (const el of els) {
                        const t = (el.textContent || '').trim();
                        if (t.length > 100) continue;  // skip large containers
                        if (t.includes('@')) continue;  // skip email-like text
                        if (t.includes("can't use") || t.includes('other way') ||
                            t.includes('Sign in another way') || t.includes('different method') ||
                            t.includes('I want to use a different') || t.includes('Use a different')) {
                            el.click();
                            return t.substring(0, 60);
                        }
                    }
                    return null;
                }""")
                if alt:
                    log.info(f"[*]  -> Clicked alt link: '{alt}'")
                    page.wait_for_timeout(3000)
                    continue

                # Try Back button as last resort
                try:
                    back = page.locator("#idBtn_Back")
                    if back.is_visible(timeout=1000):
                        log.info("[*]  -> Clicking 'Back' to exit FIDO2...")
                        back.click()
                        page.wait_for_timeout(3000)
                except Exception:
                    pass

            page.wait_for_timeout(3000)
            if found != 'not_found':
                log.info("[+] Clicked 'Use a verification code'.")
            else:
                log.warning("[!] Could not find 'Use a verification code', proceeding anyway...")

            # ── Step 4: Enter TOTP code ──
            log.info("[*] Step 4/5: Entering TOTP code...")
            page.wait_for_timeout(5000)  # extra wait for OTP input to render

            otp_input = None
            for otp_attempt in range(3):
                try:
                    otp_input = page.wait_for_selector(
                        "#idTxtBx_SAOTCC_OTC, input[name='otc'], input[type='tel'], input[type='number']",
                        timeout=10000,
                    )
                    if otp_input and otp_input.is_visible():
                        break
                except Exception:
                    log.info(f"[*] OTP input not ready, retry {otp_attempt+1}/3...")
                    page.wait_for_timeout(3000)

            if not otp_input:
                raise Exception("OTP input field not found after retries")

            code = totp.now()
            otp_input.fill(code)
            page.wait_for_timeout(500)
            page.click("#idSubmit_SAOTCC_Continue")
            page.wait_for_load_state("networkidle", timeout=15000)
            log.info(f"[+] TOTP code: {code}")

            # ── Step 5: "Stay signed in?" → Yes ──
            log.info("[*] Step 5/5: Stay signed in...")
            try:
                page.wait_for_selector("#idSIButton9", timeout=10000)
                page.click("#idSIButton9")
                log.info("[+] Clicked 'Yes'.")
            except Exception:
                pass

            # ── Wait for DSID cookie (deadline-aware) ──
            log.info("[*] Waiting for VPN session cookie...")
            dsid = None
            session_confirmed = False
            i = 0
            while time.monotonic() < deadline:
                page.wait_for_timeout(1000)
                i += 1

                # Handle "other user sessions in progress" confirmation page
                if not session_confirmed and ("user-confirm" in page.url or "user%2Dconfirm" in page.url):
                    try:
                        clicked = page.evaluate("""() => {
                            const els = document.querySelectorAll('input[type=submit], button, a');
                            for (const el of els) {
                                const txt = (el.value || el.textContent || '').trim();
                                if (txt.includes('Continue')) {
                                    el.click();
                                    return txt;
                                }
                            }
                            return null;
                        }""")
                        if clicked:
                            log.info(f"[*] Existing session detected, clicked '{clicked}'")
                            page.wait_for_timeout(3000)
                            session_confirmed = True
                    except Exception:
                        pass

                cookies = context.cookies("https://remote.ust.hk")
                for cookie in cookies:
                    if cookie["name"] == "DSID":
                        dsid = cookie["value"]
                        break
                if dsid:
                    break
                if i % 10 == 0:
                    remaining = int(deadline - time.monotonic())
                    log.info(f"[*]  ... still waiting ({i}s elapsed, {remaining}s left)")

        except Exception as e:
            log.error(f"[!] Automated login error: {e}")
            dsid = None
            if headless:
                log.warning("[!] Headless 模式，跳过手动登录 fallback")
            else:
                log.info("[*] Falling back to manual login. Complete in the browser.")
                while time.monotonic() < deadline:
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
        log.info(f"[+] Got DSID cookie: {dsid[:20]}...")
        return dsid
    else:
        log.error("[-] Failed to get DSID cookie.")
        return None


# ─── VPN Connection ───────────────────────────────────────────────────

def dns_query_udp(hostname, dns_server, timeout=5):
    """Resolve hostname via a specific DNS server using raw UDP."""
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.settimeout(timeout)
    try:
        qname = b''
        for part in hostname.split('.'):
            qname += bytes([len(part)]) + part.encode()
        qname += b'\x00'
        query = struct.pack('>HHHHHH', 0x1234, 0x0100, 1, 0, 0, 0) + qname + struct.pack('>HH', 1, 1)
        s.sendto(query, (dns_server, 53))
        data, _ = s.recvfrom(512)
        offset = 12
        while data[offset] != 0:
            offset += data[offset] + 1
        offset += 5
        if len(data) > offset + 12:
            offset += 2 + 8
            offset += 2
            return '.'.join(str(b) for b in data[offset:offset+4])
    except Exception:
        return None
    finally:
        s.close()


def fix_vpn_dns(hosts, sudo_password, vpn_dns="143.89.14.7", retries=10):
    """Resolve hosts via VPN DNS and add /etc/hosts entries + routes.

    vpn-slice can't resolve correctly when Clash intercepts DNS, so we
    do it manually after the tunnel is up.
    """
    resolved = set()
    for attempt in range(retries):
        time.sleep(3)
        for host in hosts:
            if host in resolved:
                continue
            ip = dns_query_udp(host, vpn_dns)
            if ip:
                marker = f"# hkust-vpn {host}"
                # Remove old entry, add fresh one, add route
                cmd = (
                    f"sed -i '/{marker}/d' /etc/hosts && "
                    f"echo '{ip} {host}  {marker}' >> /etc/hosts && "
                    f"ip route replace {ip}/32 dev tun0"
                )
                subprocess.run(
                    ["sudo", "-S", "bash", "-c", cmd],
                    input=(sudo_password + "\n").encode(),
                    capture_output=True,
                )
                log.info(f"[+] DNS fix: {host} -> {ip} (route via tun0)")
                resolved.add(host)
        if resolved == set(hosts):
            return True
    if not resolved:
        log.warning("[!] DNS fix: could not resolve any hosts via VPN DNS")
    return len(resolved) > 0


def _health_check(hosts, interval=HEALTH_CHECK_INTERVAL):
    """Periodically verify VPN tunnel passes traffic."""
    time.sleep(30)  # give tunnel time to establish
    while True:
        reachable = False
        for host in hosts:
            try:
                sock = socket.create_connection((host, 22), timeout=10)
                sock.close()
                reachable = True
                break
            except (socket.timeout, OSError):
                continue
        if not reachable:
            log.warning(f"[!] Health check FAILED: none of {hosts} reachable on port 22")
        time.sleep(interval)


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

    log.info(f"[*] Connecting VPN (split tunnel: {', '.join(hosts)})")

    try:
        proc = subprocess.Popen(
            cmd,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,  # merge stderr into stdout
        )
        # Send sudo password
        if sudo_password:
            proc.stdin.write((sudo_password + "\n").encode())
            proc.stdin.flush()

        # Fix DNS in background after tunnel comes up
        dns_thread = threading.Thread(
            target=fix_vpn_dns,
            args=(hosts, sudo_password),
            daemon=True,
        )
        dns_thread.start()

        # Health check in background
        health_thread = threading.Thread(
            target=_health_check,
            args=(hosts,),
            daemon=True,
        )
        health_thread.start()

        # Read and log openconnect output
        for line in proc.stdout:
            text = line.decode("utf-8", errors="replace").rstrip()
            if text:
                log.info(f"[openconnect] {text}")

        rc = proc.wait()
        log.info(f"[*] openconnect exited with code {rc}")

    except KeyboardInterrupt:
        log.info("\n[*] VPN disconnected.")
        raise  # re-raise so auto_reconnect knows it was user-initiated


def auto_reconnect(user, password, totp_secret, sudo_password,
                   proxy=None, hosts=None, headless=False):
    """Auto-reconnect loop with exponential backoff on login failures.

    HKUST VPN limits:
      - Inactivity timeout: 30 min
      - Max session length: 240 min (4 hours)
    """
    attempt = 0
    consecutive_login_failures = 0

    while True:
        attempt += 1
        start = time.time()
        log.info(f"\n{'='*50}")
        log.info(f"[*] VPN session #{attempt} starting at {time.strftime('%H:%M:%S')}")
        log.info(f"[*] Session will expire in ~4 hours (240 min)")
        log.info(f"{'='*50}\n")

        try:
            dsid = get_dsid_cookie(user, password, totp_secret,
                                   proxy=proxy, headless=headless)
        except KeyboardInterrupt:
            log.info("\n[*] User interrupted. Exiting.")
            break

        if dsid is None:
            consecutive_login_failures += 1
            log.warning(f"[!] Login failed ({consecutive_login_failures}/{MAX_CONSECUTIVE_FAILURES})")

            if consecutive_login_failures >= MAX_CONSECUTIVE_FAILURES:
                log.error(f"[-] {MAX_CONSECUTIVE_FAILURES} consecutive login failures. Giving up.")
                sys.exit(1)

            # Exponential backoff with jitter
            delay = min(BACKOFF_BASE * (2 ** (consecutive_login_failures - 1)), BACKOFF_CAP)
            jitter = random.uniform(0, delay * 0.3)
            wait = delay + jitter
            log.info(f"[*] Retrying in {wait:.0f}s (backoff)...")

            try:
                time.sleep(wait)
            except KeyboardInterrupt:
                log.info("\n[*] User interrupted. Exiting.")
                break
            continue

        # Login succeeded — reset failure counter
        consecutive_login_failures = 0

        try:
            connect_vpn(dsid, proxy=proxy, hosts=hosts,
                        sudo_password=sudo_password)
        except KeyboardInterrupt:
            log.info("\n[*] User interrupted. Exiting.")
            break

        elapsed = time.time() - start
        log.info(f"\n[!] VPN disconnected after {elapsed/60:.1f} minutes.")

        if elapsed < 30:
            wait = 10
            log.warning(f"[!] Session too short. Waiting {wait}s before retry...")
        else:
            wait = 5
            log.info(f"[*] Reconnecting in {wait}s... (Ctrl+C to stop)")

        try:
            time.sleep(wait)
        except KeyboardInterrupt:
            log.info("\n[*] User interrupted. Exiting.")
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
    if not args.user:
        args.user = creds.get("user", "")
    if not args.user:
        args.user = input("[?] ITSC account (e.g. user@connect.ust.hk): ").strip()
    if args.setup or not creds.get("password") or not creds.get("totp_secret"):
        password, totp_secret, sudo_password = setup_credentials(args.user)
    else:
        password = creds["password"]
        totp_secret = creds["totp_secret"]
        sudo_password = creds.get("sudo_password", "")
        args.user = creds.get("user", args.user)
        if not sudo_password:
            sudo_password = getpass.getpass("[?] sudo password: ")

    log.info(f"[*] Log file: {LOG_FILE}")

    if args.cookie:
        dsid = args.cookie
        connect_vpn(dsid, proxy=proxy, hosts=args.hosts, sudo_password=sudo_password)
    elif args.no_reconnect:
        dsid = get_dsid_cookie(
            args.user, password, totp_secret,
            proxy=proxy, headless=args.headless,
        )
        if dsid is None:
            log.error("[-] Login failed. Exiting.")
            sys.exit(1)
        connect_vpn(dsid, proxy=proxy, hosts=args.hosts, sudo_password=sudo_password)
    else:
        auto_reconnect(
            args.user, password, totp_secret, sudo_password,
            proxy=proxy, hosts=args.hosts, headless=args.headless,
        )


if __name__ == "__main__":
    main()
