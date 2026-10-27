#!/usr/bin/env python3
"""Machine-safety guard: run before every heavy step.

Prints CPU %, RAM %, VRAM used/total, GPU temperature and free disk, then
exits 1 if any hard-stop limit from PLAN.md is exceeded.

Hard stops:  RAM > 85%  |  VRAM > 85%  |  CPU > 90% sustained over 60 s
             GPU temp >= 85 C  |  free disk < 2 GB
Safe targets (warnings only): RAM <= 75%, VRAM <= 75%, GPU < 80 C,
             free disk >= 5 GB (enforced as a hard stop with --install).

Only the standard library and psutil are used. GPU checks are skipped
gracefully when nvidia-smi is not available.
"""
from __future__ import annotations

import argparse
import shutil
import subprocess
import sys
import time

import psutil

RAM_SAFE, RAM_STOP = 75.0, 85.0
VRAM_SAFE, VRAM_STOP = 75.0, 85.0
CPU_STOP, CPU_SUSTAIN_S = 90.0, 60
GPU_TEMP_SAFE, GPU_TEMP_STOP, GPU_TEMP_RESUME = 80, 85, 75
DISK_SAFE_GB, DISK_STOP_GB = 5.0, 2.0


def query_gpus() -> list[dict]:
    """Return one dict per GPU, or [] when nvidia-smi is missing or fails."""
    exe = shutil.which("nvidia-smi")
    if not exe:
        return []
    try:
        out = subprocess.run(
            [exe, "--query-gpu=name,memory.used,memory.total,utilization.gpu,temperature.gpu",
             "--format=csv,noheader,nounits"],
            capture_output=True, text=True, timeout=15, check=True,
        ).stdout
    except (subprocess.SubprocessError, OSError):
        return []
    gpus = []
    for line in out.strip().splitlines():
        parts = [p.strip() for p in line.split(",")]
        if len(parts) != 5:
            continue
        try:
            gpus.append({
                "name": parts[0],
                "used_mb": float(parts[1]),
                "total_mb": float(parts[2]),
                "util": float(parts[3]),
                "temp": float(parts[4]),
            })
        except ValueError:
            continue
    return gpus


def sample_cpu(quick_s: float) -> tuple[float, bool]:
    """Sample CPU; if the quick sample is above the stop line, keep sampling
    for the full sustain window. Returns (percent, sustained_over_limit)."""
    pct = psutil.cpu_percent(interval=quick_s)
    if pct <= CPU_STOP:
        return pct, False
    print(f"  CPU {pct:.0f}% > {CPU_STOP:.0f}% on quick sample; sampling {CPU_SUSTAIN_S}s to check if sustained...")
    pct = psutil.cpu_percent(interval=CPU_SUSTAIN_S)
    return pct, pct > CPU_STOP


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--path", default=".", help="filesystem path whose free space is checked")
    ap.add_argument("--install", action="store_true",
                    help="before an install/download: require >= 5 GB free disk")
    ap.add_argument("--wait-cool", action="store_true",
                    help="if GPU temp >= 85 C, wait (up to 10 min) until < 75 C instead of failing")
    ap.add_argument("--quick", type=float, default=2.0, help="seconds for the quick CPU sample")
    args = ap.parse_args()

    failures: list[str] = []
    warnings: list[str] = []

    cpu, cpu_sustained = sample_cpu(args.quick)
    mem = psutil.virtual_memory()
    disk_free_gb = shutil.disk_usage(args.path).free / 1e9

    print(f"CPU   : {cpu:5.1f}%  ({psutil.cpu_count(logical=True)} logical cores)")
    print(f"RAM   : {mem.percent:5.1f}%  used {(mem.total - mem.available) / 2**20:,.0f} MB of {mem.total / 2**20:,.0f} MB")
    print(f"Disk  : {disk_free_gb:,.1f} GB free at {args.path}")

    if cpu_sustained:
        failures.append(f"CPU sustained {cpu:.0f}% > {CPU_STOP:.0f}% for {CPU_SUSTAIN_S}s")
    if mem.percent > RAM_STOP:
        failures.append(f"RAM {mem.percent:.0f}% > {RAM_STOP:.0f}%")
    elif mem.percent > RAM_SAFE:
        warnings.append(f"RAM {mem.percent:.0f}% above safe target {RAM_SAFE:.0f}%")
    if disk_free_gb < DISK_STOP_GB:
        failures.append(f"free disk {disk_free_gb:.1f} GB < {DISK_STOP_GB} GB")
    elif disk_free_gb < DISK_SAFE_GB:
        msg = f"free disk {disk_free_gb:.1f} GB < {DISK_SAFE_GB} GB"
        (failures if args.install else warnings).append(msg)

    gpus = query_gpus()
    if not gpus:
        print("GPU   : nvidia-smi not available - GPU checks skipped")
    for i, g in enumerate(gpus):
        vram_pct = 100.0 * g["used_mb"] / g["total_mb"] if g["total_mb"] else 0.0
        if g["temp"] >= GPU_TEMP_STOP and args.wait_cool:
            deadline = time.time() + 600
            while g["temp"] >= GPU_TEMP_RESUME and time.time() < deadline:
                print(f"  GPU{i} {g['temp']:.0f} C - pausing until < {GPU_TEMP_RESUME} C ...")
                time.sleep(15)
                fresh = query_gpus()
                g = fresh[i] if i < len(fresh) else g
        print(f"GPU{i}  : {g['name']} | VRAM {g['used_mb']:.0f}/{g['total_mb']:.0f} MiB ({vram_pct:.1f}%) "
              f"| util {g['util']:.0f}% | temp {g['temp']:.0f} C")
        if vram_pct > VRAM_STOP:
            failures.append(f"GPU{i} VRAM {vram_pct:.0f}% > {VRAM_STOP:.0f}%")
        elif vram_pct > VRAM_SAFE:
            warnings.append(f"GPU{i} VRAM {vram_pct:.0f}% above safe target {VRAM_SAFE:.0f}%")
        if g["temp"] >= GPU_TEMP_STOP:
            failures.append(f"GPU{i} temp {g['temp']:.0f} C >= {GPU_TEMP_STOP} C")
        elif g["temp"] >= GPU_TEMP_SAFE:
            warnings.append(f"GPU{i} temp {g['temp']:.0f} C above safe target {GPU_TEMP_SAFE} C")

    for w in warnings:
        print(f"WARN  : {w}")
    if failures:
        for f in failures:
            print(f"STOP  : {f}")
        print("RESULT: FAIL - do not start heavy work")
        return 1
    print("RESULT: OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
