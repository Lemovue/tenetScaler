#!/usr/bin/env python3
"""Run one heavy step (build, test, install, server) safely.

    python scripts/dev/heavy.py [--timeout SECONDS] [--install] -- <command> [args...]

1. Acquires a cross-process lock (scripts/dev/.heavy.lock) so only ONE heavy
   step runs at a time on this machine, even across parallel agents.
2. Runs resource_guard.py; if it fails, waits and re-checks (up to 5 minutes)
   instead of starting.
3. Runs the command with parallelism caps in its environment
   (GOMAXPROCS, GOFLAGS=-p=4, OMP/MKL threads, Node heap) and a hard timeout.
4. Releases the lock and exits with the command's exit code.
"""
from __future__ import annotations

import argparse
import os
import subprocess
import sys
import time
from pathlib import Path

import psutil

HERE = Path(__file__).resolve().parent
LOCK = HERE / ".heavy.lock"
GUARD = HERE / "resource_guard.py"
N_JOBS = 8          # half of the 16 logical cores (PLAN.md)
GO_PARALLEL = 4     # RAM-bound for client-go compiles (PLAN.md)
NODE_HEAP_MB = 4000  # ~25% of 16 GB


def acquire_lock(wait_s: float) -> None:
    deadline = time.time() + wait_s
    while True:
        try:
            fd = os.open(LOCK, os.O_CREAT | os.O_EXCL | os.O_WRONLY)
            os.write(fd, str(os.getpid()).encode())
            os.close(fd)
            return
        except FileExistsError:
            try:
                holder = int(LOCK.read_text().strip() or "0")
            except (OSError, ValueError):
                holder = 0
            if holder and not psutil.pid_exists(holder):
                print(f"[heavy] removing stale lock held by dead pid {holder}", flush=True)
                LOCK.unlink(missing_ok=True)
                continue
            if time.time() > deadline:
                raise SystemExit(f"[heavy] timed out waiting for lock held by pid {holder}")
            time.sleep(2)


def release_lock() -> None:
    try:
        if LOCK.read_text().strip() == str(os.getpid()):
            LOCK.unlink()
    except OSError:
        pass


def guard_ok(install: bool) -> bool:
    cmd = [sys.executable, str(GUARD), "--wait-cool"]
    if install:
        cmd.append("--install")
    return subprocess.run(cmd).returncode == 0


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--timeout", type=float, default=900, help="hard timeout for the command (s)")
    ap.add_argument("--install", action="store_true", help="also require >= 5 GB free disk")
    ap.add_argument("--lock-wait", type=float, default=3600, help="max seconds to wait for the lock")
    ap.add_argument("cmd", nargs=argparse.REMAINDER)
    args = ap.parse_args()
    cmd = args.cmd[1:] if args.cmd and args.cmd[0] == "--" else args.cmd
    if not cmd:
        ap.error("no command given (use: heavy.py -- <command>)")

    acquire_lock(args.lock_wait)
    try:
        for attempt in range(11):
            if guard_ok(args.install):
                break
            if attempt == 10:
                print("[heavy] resource guard still failing after 5 minutes - not starting", flush=True)
                return 1
            print("[heavy] guard failed - waiting 30 s before re-checking", flush=True)
            time.sleep(30)

        env = dict(os.environ)
        env.setdefault("GOMAXPROCS", str(N_JOBS))
        env["GOFLAGS"] = (env.get("GOFLAGS", "") + f" -p={GO_PARALLEL}").strip()
        env.setdefault("OMP_NUM_THREADS", str(N_JOBS))
        env.setdefault("MKL_NUM_THREADS", str(N_JOBS))
        env.setdefault("TOKENIZERS_PARALLELISM", "false")
        env.setdefault("NODE_OPTIONS", f"--max-old-space-size={NODE_HEAP_MB}")

        print(f"[heavy] running (timeout {args.timeout:.0f}s): {' '.join(cmd)}", flush=True)
        proc = subprocess.Popen(cmd, env=env)
        try:
            return proc.wait(timeout=args.timeout)
        except subprocess.TimeoutExpired:
            print(f"[heavy] TIMEOUT after {args.timeout:.0f}s - killing process tree", flush=True)
            try:
                parent = psutil.Process(proc.pid)
                for child in parent.children(recursive=True):
                    child.kill()
                parent.kill()
            except psutil.NoSuchProcess:
                pass
            return 124
    finally:
        release_lock()


if __name__ == "__main__":
    sys.exit(main())
