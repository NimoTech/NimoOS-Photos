"""Benchmark mlserver against the live immich-ml container.

Two things are measured, as two subcommands (run in this order so the
production immich-ml container is only ever restarted last):

  steady      Per task-family (clip visual, clip textual, face, ocr)
              p50/p95 latency + throughput against BOTH servers, using
              golden dataset images/queries round-robin with identical
              payloads. Both servers must already be warm and reachable.

  cold-start  Time from process start to first successful /predict
              (clip visual), plus /ping p99 sampled concurrently while
              the model is still loading. Two targets:
                --target ours    launches a fresh uvicorn subprocess
                                  (given via --cmd) and measures it.
                --target immich  `docker restart <--container>`, waits
                                  for /ping, then times first /predict.
                                  Run this one LAST -- it's the
                                  production container.

Each subcommand reads/merges its results into --out (default
golden/report-bench.json) rather than overwriting it, so `steady`,
`cold-start --target ours`, and `cold-start --target immich` can be run
as three separate invocations and still end up in one report file.
"""
import argparse
import json
import socket
import statistics
import subprocess
import sys
import threading
import time
from pathlib import Path

import numpy as np
import requests

CLIP_MODEL = "ViT-SO400M-16-SigLIP2-384__webli"
FACE_MODEL = "antelopev2"
OCR_MODEL = "PP-OCRv5_server"


def load_out(path: Path) -> dict:
    if path.exists():
        return json.loads(path.read_text())
    return {}


def save_out(path: Path, data: dict) -> None:
    path.write_text(json.dumps(data, indent=2))


def load_payloads(dataset: Path):
    manifest = json.loads((dataset / "manifest.json").read_text())
    queries = json.loads((dataset / "queries.json").read_text())
    images = [(dataset / info["file"]).read_bytes() for info in manifest.values()]
    return images, queries


# -- one request per task family --------------------------------------------

def _predict(base_url: str, entries: dict, image: bytes | None = None,
             text: str | None = None, timeout: float = 120.0) -> float:
    """POST /predict, return wall-clock seconds. Raises on non-2xx."""
    data = {"entries": json.dumps(entries)}
    files = {"image": ("image.jpg", image, "application/octet-stream")} if image is not None else None
    if text is not None:
        data["text"] = text
    t0 = time.perf_counter()
    r = requests.post(f"{base_url}/predict", data=data, files=files, timeout=timeout)
    dt = time.perf_counter() - t0
    r.raise_for_status()
    return dt


def clip_visual_req(base_url: str, image: bytes) -> float:
    return _predict(base_url, {"clip": {"visual": {"modelName": CLIP_MODEL}}}, image=image)


def clip_textual_req(base_url: str, text: str) -> float:
    return _predict(base_url, {"clip": {"textual": {"modelName": CLIP_MODEL}}}, text=text)


def face_req(base_url: str, image: bytes) -> float:
    return _predict(base_url, {"facial-recognition": {
        "detection": {"modelName": FACE_MODEL}, "recognition": {"modelName": FACE_MODEL}}}, image=image)


def ocr_req(base_url: str, image: bytes) -> float:
    return _predict(base_url, {"ocr": {
        "detection": {"modelName": OCR_MODEL}, "recognition": {"modelName": OCR_MODEL}}}, image=image)


FAMILIES = {
    "clip_visual": ("image", clip_visual_req),
    "clip_textual": ("text", clip_textual_req),
    "face": ("image", face_req),
    "ocr": ("image", ocr_req),
}


def _run_family(base_url: str, kind: str, fn, payloads: list, n: int, concurrency: int) -> dict:
    # One untimed warmup request so a cold model load never pollutes the
    # steady-state numbers (cold start is measured separately below).
    fn(base_url, payloads[0])

    jobs = [payloads[i % len(payloads)] for i in range(n)]
    latencies: list[float] = []
    lock = threading.Lock()
    errors = []

    def worker(payload):
        try:
            dt = fn(base_url, payload)
            with lock:
                latencies.append(dt)
        except Exception as e:  # noqa: BLE001 -- record and keep going
            with lock:
                errors.append(str(e))

    t0 = time.perf_counter()
    if concurrency <= 1:
        for p in jobs:
            worker(p)
    else:
        from concurrent.futures import ThreadPoolExecutor
        with ThreadPoolExecutor(max_workers=concurrency) as ex:
            list(ex.map(worker, jobs))
    wall = time.perf_counter() - t0

    arr = np.asarray(latencies) if latencies else np.asarray([float("nan")])
    return {
        "n": n, "ok": len(latencies), "errors": len(errors),
        "error_samples": errors[:3],
        "p50_ms": float(np.percentile(arr, 50) * 1000),
        "p95_ms": float(np.percentile(arr, 95) * 1000),
        "mean_ms": float(arr.mean() * 1000),
        "wall_s": wall,
        "throughput_rps": len(latencies) / wall if wall > 0 else 0.0,
        "concurrency": concurrency,
    }


def cmd_steady(args: argparse.Namespace) -> None:
    images, queries = load_payloads(Path(args.dataset))
    out = load_out(Path(args.out))
    out.setdefault("steady", {})
    for label, base_url in (("ours", args.ours), ("immich", args.immich)):
        out["steady"].setdefault(label, {})
        for family, (kind, fn) in FAMILIES.items():
            payloads = queries if kind == "text" else images
            print(f"[{label}] {family}: n={args.n} concurrency={args.concurrency} ...", file=sys.stderr)
            result = _run_family(base_url, family, fn, payloads, args.n, args.concurrency)
            out["steady"][label][family] = result
            print(f"  p50={result['p50_ms']:.1f}ms p95={result['p95_ms']:.1f}ms "
                  f"throughput={result['throughput_rps']:.2f}rps "
                  f"errors={result['errors']}/{result['n']}", file=sys.stderr)
    save_out(Path(args.out), out)
    print(f"wrote {args.out}", file=sys.stderr)


# -- cold start ---------------------------------------------------------------

def _wait_port(host: str, port: int, deadline: float) -> None:
    while time.monotonic() < deadline:
        try:
            with socket.create_connection((host, port), timeout=0.5):
                return
        except OSError:
            time.sleep(0.05)
    raise TimeoutError(f"{host}:{port} never opened")


def _sample_ping(base_url: str, stop: threading.Event, samples: list,
                  during_load: list, load_done: threading.Event) -> None:
    """Hammer /ping as fast as it responds (T6's methodology: a "500-sample
    hammer" spanning a real cold load, not a fixed-interval poll) -- records
    every latency in `samples`, and separately tags the subset that landed
    while `load_done` was not yet set in `during_load` (T6's own finding:
    only the ONE ping overlapping the InferenceSession C++ constructor's
    GIL-held span stalls; the rest of even a during-load window stay fast)."""
    while not stop.is_set():
        try:
            t0 = time.perf_counter()
            r = requests.get(f"{base_url}/ping", timeout=5.0)
            dt = time.perf_counter() - t0
            if r.status_code == 200:
                samples.append(dt)
                if not load_done.is_set():
                    during_load.append(dt)
        except Exception:
            pass


def _cold_start_measure(base_url: str, host: str, port: int, t0: float,
                         warmup_deadline_s: float, image: bytes,
                         post_load_hammer_s: float = 3.0) -> dict:
    _wait_port(host, port, time.monotonic() + warmup_deadline_s)
    t_listen = time.perf_counter()

    stop = threading.Event()
    load_done = threading.Event()
    ping_samples: list = []
    ping_during_load: list = []
    ping_thread = threading.Thread(target=_sample_ping,
                                    args=(base_url, stop, ping_samples, ping_during_load, load_done))
    ping_thread.start()

    predict_result: dict = {}

    # First successful /predict, timed from process start (t0), not from
    # port-open -- matches "time from process start to first successful
    # /predict" in the task brief.
    t_predict_start = time.perf_counter()
    # A port that accepts TCP connections doesn't mean the app behind it is
    # actually serving yet (seen in practice right after `docker restart`:
    # a brief window where the socket is open but the app resets the
    # connection) -- retry on connection-level errors, not just timeouts.
    predict_deadline = time.monotonic() + warmup_deadline_s
    while True:
        try:
            r = requests.post(f"{base_url}/predict",
                               data={"entries": json.dumps({"clip": {"visual": {"modelName": CLIP_MODEL}}})},
                               files={"image": ("image.jpg", image, "application/octet-stream")},
                               timeout=600)
            r.raise_for_status()
            predict_result["ok"] = True
            break
        except (requests.exceptions.ConnectionError, requests.exceptions.HTTPError) as e:
            if time.monotonic() >= predict_deadline:
                predict_result["ok"] = False
                predict_result["error"] = str(e)
                break
            time.sleep(0.1)
        except Exception as e:
            predict_result["ok"] = False
            predict_result["error"] = str(e)
            break
    t_predict_done = time.perf_counter()
    load_done.set()

    # Keep hammering a bit past load completion so the p99 reflects a
    # realistic serving window (T6-style dilution), not just the narrow
    # load span itself where a single GIL-held stall would dominate p99.
    time.sleep(post_load_hammer_s)
    stop.set()
    ping_thread.join(timeout=5)

    arr = np.asarray(ping_samples) if ping_samples else np.asarray([float("nan")])
    during_arr = np.asarray(ping_during_load) if ping_during_load else np.asarray([float("nan")])
    return {
        "cold_start_s": t_predict_done - t0,
        "port_open_s": t_listen - t0,
        "predict_wait_s": t_predict_done - t_predict_start,
        "predict_ok": predict_result.get("ok", False),
        "predict_error": predict_result.get("error"),
        # Full window (load span + post_load_hammer_s dilution), T6-style.
        "ping_samples_total": len(ping_samples),
        "ping_p50_ms": float(np.percentile(arr, 50) * 1000) if ping_samples else None,
        "ping_p99_ms": float(np.percentile(arr, 99) * 1000) if ping_samples else None,
        "ping_max_ms": float(arr.max() * 1000) if ping_samples else None,
        # Load span only -- isolates the InferenceSession-construction stall
        # T6 root-caused (GIL held during the C++ constructor) without
        # dilution from the post-load hammering window.
        "ping_samples_during_load": len(ping_during_load),
        "ping_p99_during_load_ms": float(np.percentile(during_arr, 99) * 1000) if ping_during_load else None,
        "ping_max_during_load_ms": float(during_arr.max() * 1000) if ping_during_load else None,
    }


def cmd_cold_ours(args: argparse.Namespace) -> None:
    import os

    images, _ = load_payloads(Path(args.dataset))
    image = images[0]
    env = os.environ.copy()
    env["MLSERVER_CACHE"] = args.cache
    env["MLSERVER_DEVICE"] = args.device
    env["MLSERVER_TTL"] = "300"

    logf = open(args.log, "w")
    t0 = time.perf_counter()
    proc = subprocess.Popen(
        [args.python, "-m", "uvicorn", "server.main:app", "--host", args.host, "--port", str(args.port)],
        cwd=args.cwd, env=env, stdout=logf, stderr=subprocess.STDOUT,
    )
    try:
        result = _cold_start_measure(f"http://{args.host}:{args.port}", args.host, args.port,
                                      t0, args.startup_timeout, image)
        result["label"] = args.label
        result["device"] = args.device
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
        logf.close()

    out = load_out(Path(args.out))
    out.setdefault("cold_start", {}).setdefault("ours", {})[args.label] = result
    save_out(Path(args.out), out)
    print(json.dumps(result, indent=2))


def cmd_cold_immich(args: argparse.Namespace) -> None:
    images, _ = load_payloads(Path(args.dataset))
    image = images[0]
    host, port = args.host, args.port

    t0 = time.perf_counter()
    subprocess.run(["docker", "restart", args.container], check=True,
                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    result = _cold_start_measure(f"http://{host}:{port}", host, port, t0, args.startup_timeout, image)

    # Confirm /ping recovers post-restart before handing control back --
    # this container serves production traffic.
    r = requests.get(f"http://{host}:{port}/ping", timeout=10)
    result["ping_recovered"] = (r.status_code == 200 and "pong" in r.text)

    out = load_out(Path(args.out))
    out.setdefault("cold_start", {})["immich"] = result
    save_out(Path(args.out), out)
    print(json.dumps(result, indent=2))
    if not result["ping_recovered"]:
        print("WARNING: immich /ping did not recover cleanly after restart!", file=sys.stderr)
        sys.exit(1)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    sub = ap.add_subparsers(dest="cmd", required=True)

    p_steady = sub.add_parser("steady", help="p50/p95 latency + throughput, both servers")
    p_steady.add_argument("--dataset", default="golden/dataset")
    p_steady.add_argument("--ours", default="http://127.0.0.1:3004")
    p_steady.add_argument("--immich", default="http://127.0.0.1:3003")
    p_steady.add_argument("--n", type=int, default=80)
    p_steady.add_argument("--concurrency", type=int, default=4)
    p_steady.add_argument("--out", default="golden/report-bench.json")
    p_steady.set_defaults(func=cmd_steady)

    p_cold_ours = sub.add_parser("cold-ours", help="cold start our own uvicorn process")
    p_cold_ours.add_argument("--dataset", default="golden/dataset")
    p_cold_ours.add_argument("--cwd", default=".")
    p_cold_ours.add_argument("--python", default=sys.executable, help="interpreter to launch uvicorn with")
    p_cold_ours.add_argument("--cache", required=True, help="MLSERVER_CACHE for this run")
    p_cold_ours.add_argument("--device", default="auto")
    p_cold_ours.add_argument("--host", default="127.0.0.1")
    p_cold_ours.add_argument("--port", type=int, default=3004)
    p_cold_ours.add_argument("--startup-timeout", type=float, default=30.0)
    p_cold_ours.add_argument("--label", default="warm-ov-cache",
                              help="tag this run in the report, e.g. warm-ov-cache | first-compile")
    p_cold_ours.add_argument("--log", default="golden/.cold-ours.log")
    p_cold_ours.add_argument("--out", default="golden/report-bench.json")
    p_cold_ours.set_defaults(func=cmd_cold_ours)

    p_cold_immich = sub.add_parser("cold-immich", help="docker restart immich-ml and measure (run LAST)")
    p_cold_immich.add_argument("--dataset", default="golden/dataset")
    p_cold_immich.add_argument("--container", required=True)
    p_cold_immich.add_argument("--host", default="127.0.0.1")
    p_cold_immich.add_argument("--port", type=int, default=3003)
    p_cold_immich.add_argument("--startup-timeout", type=float, default=60.0)
    p_cold_immich.add_argument("--out", default="golden/report-bench.json")
    p_cold_immich.set_defaults(func=cmd_cold_immich)

    args = ap.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
