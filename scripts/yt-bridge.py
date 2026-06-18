#!/usr/bin/env python3
"""
yt-bridge: Opens Chrome on YouTube for cookie extraction via noVNC.
Keeps a headed Chrome session alive that can be logged into YouTube,
so auto-cookie-refresh.sh can extract cookies via CDP.

Requires:
  - Xvfb (display :99)
  - x11vnc (no password, bound to localhost)
  - websockify (port 6080 → 5900)
  - Playwright + playwright-stealth
  - google-chrome installed

Usage:
  DISPLAY=:99 python3 scripts/yt-bridge.py

noVNC access:
  https://vnc.mtosity.com/vnc.html  (if behind cloudflared tunnel)
  http://localhost:6080/vnc.html    (direct)
"""
import asyncio
import os
import sys
import time
import signal
from pathlib import Path

PROFILE = os.environ.get("YT_BRIDGE_PROFILE", str(Path.home() / ".openclaw/workspace/wsj-reader/chrome-profile"))
LOG_FILE = Path(os.environ.get("YT_BRIDGE_LOG", "/tmp/yt-bridge.log"))

# Ensure clean profile state
for lock in Path(PROFILE).glob("Singleton*"):
    try: lock.unlink()
    except: pass


def log(msg):
    ts = time.strftime("%Y-%m-%d %H:%M:%S")
    line = f"[{ts}] {msg}"
    print(line, file=sys.stderr, flush=True)
    with LOG_FILE.open("a") as f:
        f.write(line + "\n")


async def main():
    from playwright.async_api import async_playwright
    from playwright_stealth import Stealth

    log("Starting YouTube bridge — opening Chrome on shared profile")

    async with async_playwright() as pw:
        browser = await pw.chromium.launch_persistent_context(
            user_data_dir=PROFILE,
            headless=False, channel="chrome",
            args=[
                "--no-sandbox",
                "--disable-blink-features=AutomationControlled",
                "--disable-dev-shm-usage",
                "--disable-gpu",
                "--remote-debugging-port=9222",
            ],
            viewport={"width": 1600, "height": 900},
            locale="en-US",
            timezone_id="America/New_York",
        )
        stealth = Stealth()
        await stealth.apply_stealth_async(browser)
        log("Browser launched")

        page = await browser.new_page()
        log("Loading YouTube.com...")
        try:
            await page.goto("https://www.youtube.com/", wait_until="domcontentloaded", timeout=60000)
            await asyncio.sleep(5)
            log(f"Loaded. URL: {page.url}")
            log(f"Title: {await page.title()}")
        except Exception as e:
            log(f"Initial load error: {e}")

        stop_event = asyncio.Event()
        def handle_signal():
            log("Shutdown signal received")
            stop_event.set()
        loop = asyncio.get_event_loop()
        for sig in (signal.SIGTERM, signal.SIGINT):
            loop.add_signal_handler(sig, handle_signal)

        log("=== BRIDGE READY ===")
        log("YouTube is open in Chrome. Connect via noVNC to log in:")
        log("  https://vnc.mtosity.com/vnc.html")
        log("Bridge will keep this Chrome session alive for 2 hours.")

        try:
            for i in range(120):
                if stop_event.is_set():
                    break
                if page.is_closed():
                    log("Page closed — exiting")
                    break
                if i % 10 == 0:
                    log(f"  bridge alive: {i*60}s elapsed")
                await asyncio.sleep(60)
        except asyncio.CancelledError:
            log("Cancelled")
        finally:
            log("Closing browser")
            await browser.close()

    log("Bridge stopped")


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        log("Interrupted")
