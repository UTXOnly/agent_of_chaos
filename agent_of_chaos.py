#!/usr/bin/env python3
"""
agent_of_chaos — Datadog Logs Agent stress tester

Spawns configurable service workers, each writing an independent plain-text +
JSON rotating log pair into this repository by default (override with --log-dir).
Six execution modes:

  steady    constant load, all services start immediately
  ramp      start small, add services gradually until target is reached
  chaos     services randomly crash and restart while running
  spike     steady baseline + periodic coordinated volume spikes
  scenario  run a sequence of named phases with smooth transitions between
            them — designed for multi-hour observation runs
  pulse     repeating pressure-test cycle: gradual ramp → sustained peak →
            gradual cooldown → quiet rest period, then repeat

Usage examples:
  python3 agent_of_chaos.py --mode steady --services 10
  python3 agent_of_chaos.py --mode ramp --ramp-start 1 --ramp-step 2 --ramp-interval 20 --services 10
  python3 agent_of_chaos.py --mode chaos --services 8 --chaos-crash-rate 3
  python3 agent_of_chaos.py --mode spike --services 5 --spike-services 10
  python3 agent_of_chaos.py --mode scenario --scenario wave
  python3 agent_of_chaos.py --mode scenario --scenario business-day --scenario-speed 3
  python3 agent_of_chaos.py --mode scenario --scenario-file my_scenario.json
  python3 agent_of_chaos.py --mode scenario --scenario list
  python3 agent_of_chaos.py --mode pulse
  python3 agent_of_chaos.py --mode pulse --pulse-ramp 300 --pulse-peak 120 --pulse-rest 600

  Deterministic benchmarks (JSON config — byte-identical logs per file for a
  fixed seed + config; steady aggregate rate; stop at first of duration or
  max lines):

  python3 agent_of_chaos.py --benchmark-config benchmark_profiles.json \\
      --benchmark profile-high-throughput
  # Run one --benchmark name per agent profile (separate agent runs).  Optional:
  # --run-all-benchmarks runs every case back-to-back in one script session.

  Benchmark JSON uses a deterministic line budget: line_cap = min(max_total_lines,
  floor(duration_sec * total_lines_per_sec)) so the same config always emits the same
  number of lines (split across workers), giving byte-identical log files per run.

Stop with Ctrl+C (or when a benchmark finishes its line_cap).

Environment variables (prefix AOCH_, CLI flags take precedence):
  AOCH_MODE, AOCH_SERVICES, AOCH_LOG_DIR, AOCH_BENCHMARK_CONFIG, …
  See README.md for the full list. Docker image defaults AOCH_LOG_DIR to
  /var/log/agent-of-chaos.
"""

import argparse
import copy
import hashlib
import json
import logging
import logging.handlers
import os
import random
import signal
import sys
import threading
import time
from datetime import datetime, timedelta, timezone

# Default output directory: ./logs next to this script. Point the Logs Agent
# file tailer there; use --log-dir only if you want logs somewhere else.
DEFAULT_LOG_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "logs")

# ── Service name pool (24 independent sources) ────────────────────────────────

SERVICE_POOL = [
    "auth-service",
    "payment-service",
    "api-gateway",
    "worker",
    "cache-proxy",
    "notification-svc",
    "db-proxy",
    "metrics-collector",
    "event-bus",
    "scheduler",
    "search-indexer",
    "cdn-router",
    "session-manager",
    "audit-logger",
    "config-svc",
    "rate-limiter",
    "image-processor",
    "email-sender",
    "webhook-dispatcher",
    "data-pipeline",
    "user-service",
    "order-service",
    "inventory-service",
    "reporting-service",
]

# ── Simulated context pools ───────────────────────────────────────────────────

USERS = [
    "user_1042", "user_8871", "user_3305", "user_9912", "anonymous",
    "user_0071", "user_4420", "user_6634", "svc-account", "bot_crawler",
]
ENDPOINTS = [
    "/api/v1/login", "/api/v1/orders", "/api/v1/checkout",
    "/healthz", "/metrics", "/api/v2/search", "/api/v1/profile",
    "/api/v1/notifications", "/admin/status", "/api/v1/cart",
]
REQUEST_IDS = [f"req-{i:04d}" for i in range(1, 200)]

# ── Log message templates ─────────────────────────────────────────────────────

MESSAGES = {
    logging.DEBUG: [
        "Cache lookup for key '{key}' returned {result}",
        "DB query took {ms}ms: SELECT * FROM sessions WHERE user_id='{user}'",
        "Token validation passed for {user}",
        "Request {req} routed to backend instance {instance}",
        "Retry attempt {n}/3 for downstream call to {service}",
        "Config flag 'feature_x_enabled' resolved to {val}",
        "Parsed request body: content_length={bytes} bytes",
        "gRPC stream opened to {service}, deadline={ms}ms",
        "Span {req} sampled at rate=0.{n}0, trace_id={key}",
        "Redis pipeline flushed: {n} commands in {ms}ms",
    ],
    logging.INFO: [
        "Request {req} completed: {endpoint} {status} in {ms}ms",
        "User {user} authenticated successfully",
        "Scheduled job 'cleanup_expired_sessions' started",
        "Connected to Redis at 127.0.0.1:6379",
        "Loaded {n} records from database",
        "Service {service} health check passed",
        "Deployment version 2.4.{patch} is now active",
        "Signed JWT issued to {user}, expires in 3600s",
        "S3 upload completed: key=assets/{key}.bin size={bytes}B",
        "Feature flag rollout at {pct}% for endpoint {endpoint}",
    ],
    logging.WARNING: [
        "Response time {ms}ms exceeds threshold of 500ms for {endpoint}",
        "Rate limit approaching for user {user}: {n}0/100 requests used",
        "Retrying connection to {service} (attempt {n}/3)",
        "Deprecated API endpoint {endpoint} called by {user}",
        "Memory usage at {pct}% — consider scaling",
        "Session for {user} expires in 5 minutes",
        "Config value 'max_connections' not set, using default 100",
        "Slow consumer detected on queue '{key}': lag={ms} messages",
        "TLS cert for {endpoint} expires in {n} days",
        "Circuit breaker for {service} is HALF-OPEN",
    ],
    logging.ERROR: [
        "Failed to process payment for user {user}: timeout after {ms}ms",
        "Database connection lost — unable to execute query",
        "Request {req} failed: {endpoint} returned 503",
        "Authentication failed for {user}: invalid token",
        "File upload aborted: disk quota exceeded",
        "Worker crashed on job ID {req}: unhandled exception",
        "Could not reach {service} after {n} retries",
        "Kafka produce failed: topic={key} err=LEADER_NOT_AVAILABLE",
        "Cache write-back failed for key {key}: {ms}ms timeout",
        "Failed to deserialize message on queue {key}: bad schema",
    ],
    logging.CRITICAL: [
        "CRITICAL: Database primary node is unreachable — all writes failing",
        "CRITICAL: Out of memory — service {service} is being terminated",
        "CRITICAL: SSL certificate for {endpoint} expired",
        "CRITICAL: Message queue backlog exceeded 10,000 — dropping messages",
        "CRITICAL: Security alert — brute force detected from 192.168.1.{n}",
        "CRITICAL: Disk at {pct}% capacity — log rotation blocked",
        "CRITICAL: Consensus lost — {service} split-brain detected",
    ],
}

EXCEPTION_SCENARIOS = [
    (ValueError,             "Invalid value for field 'amount': expected float, got 'NaN'"),
    (KeyError,               "'session_token'"),
    (ConnectionRefusedError, "[Errno 111] Connection refused"),
    (TimeoutError,           "Request to payment gateway timed out after 30s"),
    (PermissionError,        "[Errno 13] Permission denied: '/var/run/agent.sock'"),
    (RuntimeError,           "Worker pool exhausted — all 16 workers busy"),
    (AttributeError,         "'NoneType' object has no attribute 'user_id'"),
    (IndexError,             "list index out of range"),
    (OSError,                "[Errno 28] No space left on device"),
    (RecursionError,         "maximum recursion depth exceeded"),
]

LEVEL_WEIGHTS = [
    logging.DEBUG, logging.DEBUG,
    logging.INFO, logging.INFO, logging.INFO,
    logging.WARNING,
    logging.ERROR,
    logging.CRITICAL,
]

WIDE_PAYLOAD = (
    "x-trace-context: " + "a1b2c3d4e5f6" * 120 + " | " + "k=v;" * 200
)


# ── Built-in scenarios ────────────────────────────────────────────────────────
#
# Each scenario is a dict with:
#   description  str     shown in --scenario list
#   loop         bool    restart from phase 0 when last phase ends
#   phases       list    ordered list of phase dicts
#
# Phase fields (all except name + duration are optional; unset fields inherit
# their current value from the running state rather than resetting):
#   name            str    label shown in status output
#   duration        float  seconds to spend in this phase (scaled by --scenario-speed)
#   services        int    target active service count
#   lines_per_cycle int    lines per worker per tight-loop cycle
#   burst_size      int    lines per burst event
#   burst_interval  float  seconds between burst events
#   cycle_sleep_ms  int    ms to sleep between worker cycles (0 = flat-out)
#   multiline_rate  float  fraction of lines that emit a stack trace (0.0–1.0)
#   transition      str    "immediate" (default) or "gradual"
#   transition_sec  float  how long the gradual ramp takes (0 = snap)

BUILTIN_SCENARIOS: dict = {

    # ── wave ──────────────────────────────────────────────────────────────────
    # 20-minute cycle alternating between a quiet trough and a busy peak.
    # Services come and go with each swing.  Good default for multi-hour runs.
    "wave": {
        "description": "Alternating 10-min quiet / 10-min peak; services grow and shrink each cycle",
        "loop": True,
        "phases": [
            {
                "name": "trough",
                "duration": 600,
                "services": 2,
                "lines_per_cycle": 4,
                "burst_size": 200,
                "burst_interval": 60,
                "transition": "gradual",
                "transition_sec": 90,
            },
            {
                "name": "peak",
                "duration": 600,
                "services": 8,
                "lines_per_cycle": 30,
                "burst_size": 1500,
                "burst_interval": 5,
                "transition": "gradual",
                "transition_sec": 90,
            },
        ],
    },

    # ── business-day ──────────────────────────────────────────────────────────
    # ~3-hour cycle that mimics real traffic patterns: overnight quiet →
    # morning ramp → morning peak → lunch dip → afternoon surge → EOD cooldown.
    # Use --scenario-speed 3 to compress into ~1 hour.
    "business-day": {
        "description": "~3 h cycle: overnight quiet → morning ramp → peak → lunch dip → afternoon surge → EOD (use --scenario-speed to compress)",
        "loop": True,
        "phases": [
            {
                "name": "overnight",
                "duration": 900,
                "services": 1,
                "lines_per_cycle": 2,
                "burst_size": 50,
                "burst_interval": 120,
                "cycle_sleep_ms": 50,
            },
            {
                "name": "morning-ramp",
                "duration": 1800,
                "services": 7,
                "lines_per_cycle": 18,
                "burst_size": 600,
                "burst_interval": 15,
                "transition": "gradual",
                "transition_sec": 600,
            },
            {
                "name": "morning-peak",
                "duration": 3600,
                "services": 10,
                "lines_per_cycle": 30,
                "burst_size": 1200,
                "burst_interval": 5,
                "transition": "gradual",
                "transition_sec": 300,
            },
            {
                "name": "lunch-dip",
                "duration": 1200,
                "services": 4,
                "lines_per_cycle": 8,
                "burst_size": 300,
                "burst_interval": 30,
                "transition": "gradual",
                "transition_sec": 300,
            },
            {
                "name": "afternoon-surge",
                "duration": 3600,
                "services": 12,
                "lines_per_cycle": 38,
                "burst_size": 2000,
                "burst_interval": 4,
                "transition": "gradual",
                "transition_sec": 300,
            },
            {
                "name": "eod-cooldown",
                "duration": 1800,
                "services": 3,
                "lines_per_cycle": 6,
                "burst_size": 200,
                "burst_interval": 60,
                "transition": "gradual",
                "transition_sec": 900,
            },
        ],
    },

    # ── incident ──────────────────────────────────────────────────────────────
    # Normal ops → sudden incident (many services, very high burst rate) →
    # slow gradual recovery back to baseline.  45-minute cycle.
    "incident": {
        "description": "45-min cycle: normal ops → sudden incident spike → slow recovery",
        "loop": True,
        "phases": [
            {
                "name": "normal",
                "duration": 900,
                "services": 4,
                "lines_per_cycle": 10,
                "burst_size": 400,
                "burst_interval": 20,
            },
            {
                "name": "incident-onset",
                "duration": 120,
                "services": 14,
                "lines_per_cycle": 55,
                "burst_size": 4000,
                "burst_interval": 2,
                "transition": "gradual",
                "transition_sec": 30,
            },
            {
                "name": "incident-peak",
                "duration": 600,
                "services": 14,
                "lines_per_cycle": 55,
                "burst_size": 4000,
                "burst_interval": 2,
            },
            {
                "name": "recovery",
                "duration": 1080,
                "services": 4,
                "lines_per_cycle": 12,
                "burst_size": 500,
                "burst_interval": 15,
                "transition": "gradual",
                "transition_sec": 900,
            },
        ],
    },

    # ── longhaul ──────────────────────────────────────────────────────────────
    # Designed for 6-hour unattended runs.  Volume drifts slowly across the
    # whole cycle with no sharp edges.  Services rotate in and out throughout.
    # Use --scenario-speed 6 to run the full arc in ~1 hour.
    "longhaul": {
        "description": "6-hour arc: slow warmup → sustained peak → sustained low → overnight (use --scenario-speed 6 for 1-hour run)",
        "loop": True,
        "phases": [
            {
                "name": "warmup",
                "duration": 3600,
                "services": 5,
                "lines_per_cycle": 10,
                "burst_size": 400,
                "burst_interval": 20,
                "cycle_sleep_ms": 10,
                "transition": "gradual",
                "transition_sec": 1800,
            },
            {
                "name": "sustained-peak",
                "duration": 7200,
                "services": 12,
                "lines_per_cycle": 35,
                "burst_size": 1500,
                "burst_interval": 5,
                "transition": "gradual",
                "transition_sec": 1800,
            },
            {
                "name": "rolling-churn",
                "duration": 3600,
                "services": 8,
                "lines_per_cycle": 20,
                "burst_size": 800,
                "burst_interval": 10,
                "transition": "gradual",
                "transition_sec": 900,
            },
            {
                "name": "sustained-low",
                "duration": 7200,
                "services": 3,
                "lines_per_cycle": 4,
                "burst_size": 150,
                "burst_interval": 90,
                "cycle_sleep_ms": 30,
                "transition": "gradual",
                "transition_sec": 1800,
            },
            {
                "name": "overnight",
                "duration": 3600,
                "services": 1,
                "lines_per_cycle": 2,
                "burst_size": 50,
                "burst_interval": 300,
                "cycle_sleep_ms": 100,
                "transition": "gradual",
                "transition_sec": 1800,
            },
        ],
    },
}


# ── Helpers ───────────────────────────────────────────────────────────────────

def _worker_rng(global_seed: int, service_name: str) -> random.Random:
    """Stable per-service RNG (do not use built-in hash() — salted per process)."""
    digest = hashlib.sha256(f"{global_seed}\0{service_name}".encode()).digest()
    return random.Random(int.from_bytes(digest[:8], "big"))


def _fill_rng(template: str, service: str, rng: random.Random) -> str:
    return template.format(
        key=f"sess:{rng.randint(1000, 9999)}",
        result=rng.choice(["HIT", "MISS"]),
        ms=rng.randint(1, 2000),
        user=rng.choice(USERS),
        req=rng.choice(REQUEST_IDS),
        instance=f"i-{rng.randint(100, 199)}",
        service=service,
        n=rng.randint(1, 9),
        val=rng.choice(["true", "false"]),
        bytes=rng.randint(64, 65536),
        endpoint=rng.choice(ENDPOINTS),
        status=rng.choice([200, 201, 204, 301, 400, 401, 403, 404, 429, 500, 502, 503]),
        patch=rng.randint(0, 19),
        pct=rng.randint(70, 99),
    )


def _fill(template: str, service: str) -> str:
    return _fill_rng(template, service, random)


# ── Deterministic timestamps (byte-identical across runs for a fixed seed) ───

_SYNTH_BASE = datetime(2000, 1, 1, 0, 0, 0, tzinfo=timezone.utc)


class SyntheticTimeFilter(logging.Filter):
    """Assigns monotonic synthetic timestamps so formatters do not use wall clock."""

    def filter(self, record: logging.LogRecord) -> bool:
        n = getattr(record, "synth_seq", None)
        if n is None:
            return True
        ts = _SYNTH_BASE + timedelta(microseconds=int(n))
        iso = ts.strftime("%Y-%m-%dT%H:%M:%S.%f")
        record.synthetic_plain_ts = iso
        record.synthetic_json_ts = iso + "+00:00"
        return True


class SyntheticPlainFormatter(logging.Formatter):
    def formatTime(self, record: logging.LogRecord, datefmt: str | None = None) -> str:
        if getattr(record, "synthetic_plain_ts", None):
            return record.synthetic_plain_ts
        return super().formatTime(record, datefmt)


class JsonFormatter(logging.Formatter):
    def __init__(self, service: str, host: str):
        super().__init__()
        self._service = service
        self._host = host

    def format(self, record: logging.LogRecord) -> str:
        if getattr(record, "synthetic_json_ts", None):
            ts = record.synthetic_json_ts
        else:
            ts = datetime.fromtimestamp(record.created, tz=timezone.utc).isoformat()
        payload = {
            "timestamp": ts,
            "level":     record.levelname,
            "logger":    record.name,
            "service":   self._service,
            "host":      self._host,
            "env":       "stress-test",
            "message":   record.getMessage(),
        }
        if record.exc_info:
            payload["exception"] = self.formatException(record.exc_info)
        return json.dumps(payload)


# ── PhaseConfig — mutable settings workers read each cycle ───────────────────

class PhaseConfig:
    """
    Shared, mutable configuration that all ServiceWorkers read from on every
    cycle.  The scenario runner (or orchestrator) updates this to change volume
    mid-run without restarting threads.
    """

    _FIELDS = ("lines_per_cycle", "burst_size", "burst_interval",
               "cycle_sleep_ms", "multiline_rate")

    def __init__(self, cfg: argparse.Namespace):
        self.lines_per_cycle: int   = cfg.lines_per_cycle
        self.burst_size: int        = cfg.burst_size
        self.burst_interval: float  = cfg.burst_interval
        self.cycle_sleep_ms: int    = cfg.cycle_sleep_ms
        self.multiline_rate: float  = cfg.multiline_rate
        self._lock = threading.Lock()

    def update(self, **kwargs):
        with self._lock:
            for k, v in kwargs.items():
                if k in self._FIELDS:
                    setattr(self, k, v)

    def snapshot(self) -> dict:
        with self._lock:
            return {f: getattr(self, f) for f in self._FIELDS}


# ── Stats ─────────────────────────────────────────────────────────────────────

class Stats:
    def __init__(self):
        self._lock     = threading.Lock()
        self.total     = 0
        self.rotations = 0
        self._window   = 0
        self._ts       = time.monotonic()

    def add(self, lines: int = 1):
        with self._lock:
            self.total   += lines
            self._window += lines

    def rotation(self):
        with self._lock:
            self.rotations += 1

    def rate_and_reset(self) -> float:
        now = time.monotonic()
        with self._lock:
            elapsed      = now - self._ts
            rate         = self._window / elapsed if elapsed > 0 else 0.0
            self._window = 0
            self._ts     = now
        return rate


# ── Rotation-aware handler ────────────────────────────────────────────────────

class TrackingRotatingHandler(logging.handlers.RotatingFileHandler):
    def __init__(self, *args, stats: Stats, **kwargs):
        super().__init__(*args, **kwargs)
        self._stats = stats

    def doRollover(self):
        super().doRollover()
        self._stats.rotation()


# ── ServiceWorker ─────────────────────────────────────────────────────────────

class ServiceWorker:
    """One thread — one service — two rotating log files (plain + JSON)."""

    def __init__(self, name: str, host: str, cfg: argparse.Namespace,
                 phase_config: PhaseConfig, stats: Stats,
                 line_quota: int | None = None):
        self.name         = name
        self.host         = host
        self.cfg          = cfg           # static config (log_dir, max_bytes, …)
        self.phase_config = phase_config  # volatile config (lines_per_cycle, …)
        self.stats        = stats
        self.started_at   = 0.0
        self._stop        = threading.Event()
        self._thread: threading.Thread | None = None
        self._logger: logging.Logger | None   = None
        self._synth_i     = 0
        self._line_quota = line_quota
        self._local_emitted = 0
        if getattr(cfg, "deterministic", False):
            self._rng = _worker_rng(int(cfg.global_seed), name)
        else:
            self._rng = random.Random()

    def _log_extra(self) -> dict:
        if not getattr(self.cfg, "deterministic", False):
            return {}
        self._synth_i += 1
        return {"synth_seq": self._synth_i}

    def start(self):
        self._logger   = self._build_logger()
        self.started_at = time.monotonic()
        self._stop.clear()
        self._thread = threading.Thread(target=self._run, name=self.name, daemon=True)
        self._thread.start()

    def stop(self):
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=5)
        if self._logger:
            for h in list(self._logger.handlers):
                h.close()
                self._logger.removeHandler(h)

    def _build_logger(self) -> logging.Logger:
        logger = logging.getLogger(f"aoc.{self.name}")
        logger.setLevel(logging.DEBUG)
        logger.propagate = False
        for h in list(logger.handlers):
            h.close()
            logger.removeHandler(h)

        if getattr(self.cfg, "deterministic", False):
            for f in list(logger.filters):
                logger.removeFilter(f)
            logger.addFilter(SyntheticTimeFilter())

        plain_fmt = SyntheticPlainFormatter(
            "%(asctime)s %(levelname)-8s [%(name)s] %(message)s",
            datefmt="%Y-%m-%dT%H:%M:%S.%f",
        ) if getattr(self.cfg, "deterministic", False) else logging.Formatter(
            "%(asctime)s %(levelname)-8s [%(name)s] %(message)s",
            datefmt="%Y-%m-%dT%H:%M:%S",
        )

        plain_h = TrackingRotatingHandler(
            os.path.join(self.cfg.log_dir, f"{self.name}.log"),
            maxBytes=self.cfg.max_bytes, backupCount=self.cfg.backup_count,
            stats=self.stats,
        )
        plain_h.setFormatter(plain_fmt)

        logger.addHandler(plain_h)
        if not getattr(self.cfg, "benchmark_plain_only", False):
            json_h = TrackingRotatingHandler(
                os.path.join(self.cfg.log_dir, f"{self.name}.json.log"),
                maxBytes=self.cfg.max_bytes, backupCount=self.cfg.backup_count,
                stats=self.stats,
            )
            json_h.setFormatter(JsonFormatter(self.name, self.host))
            logger.addHandler(json_h)
        return logger

    def _emit_one_record(self) -> int:
        """Emit one primary log line plus optional multiline / wide lines. Returns stats increment."""
        log  = self._logger
        pc   = self.phase_config
        mr   = pc.multiline_rate
        rng  = self._rng
        count = 0

        level = rng.choice(LEVEL_WEIGHTS)
        log.log(
            level,
            _fill_rng(rng.choice(MESSAGES[level]), self.name, rng),
            extra=self._log_extra(),
        )
        count += 1

        if rng.random() < mr:
            exc_cls, exc_msg = rng.choice(EXCEPTION_SCENARIOS)
            try:
                raise exc_cls(exc_msg)
            except Exception:
                log.error(
                    "Unhandled exception in %s", self.name,
                    exc_info=True, extra=self._log_extra(),
                )
                count += 1

        wl_rate = float(getattr(self.cfg, "wide_line_rate", 0.03))
        if self.cfg.wide_lines and rng.random() < wl_rate:
            log.debug("WIDE-LINE %s %s", self.name, WIDE_PAYLOAD, extra=self._log_extra())
            count += 1

        return count

    def _emit_cycle(self):
        log  = self._logger
        pc   = self.phase_config
        lpc  = pc.lines_per_cycle
        count = 0

        for _ in range(lpc):
            count += self._emit_one_record()

        self.stats.add(count)

        slp = pc.cycle_sleep_ms
        if slp > 0:
            time.sleep(slp / 1000.0)

    def _emit_burst(self):
        log  = self._logger
        pc   = self.phase_config
        size = pc.burst_size
        rng  = self._rng
        lvls = [logging.DEBUG, logging.INFO, logging.WARNING, logging.ERROR, logging.CRITICAL]
        log.warning(
            "--- BURST START service=%s lines=%d ---", self.name, size,
            extra=self._log_extra(),
        )
        for i in range(size):
            lvl = lvls[i % len(lvls)]
            log.log(
                lvl, "[burst %d/%d] %s", i + 1, size,
                _fill_rng(rng.choice(MESSAGES[lvl]), self.name, rng),
                extra=self._log_extra(),
            )
        log.warning("--- BURST END service=%s ---", self.name, extra=self._log_extra())
        self.stats.add(size + 2)

    def _run_rate_limited(self):
        """Steady aggregate target: cfg.benchmark_worker_lines_per_sec for this worker.

        Emits ``benchmark_emit_chunk`` primary records per timer slice so we are not
        paying one sleep syscall per line (which caps throughput around tens of k/s).
        """
        rate = float(self.cfg.benchmark_worker_lines_per_sec)
        chunk = max(1, int(getattr(self.cfg, "benchmark_emit_chunk", 32) or 1))
        self._logger.info(
            "worker started host=%s (rate-limited %.3f lines/s chunk=%d)", self.host, rate, chunk,
            extra=self._log_extra(),
        )
        next_t = time.perf_counter()
        while not self._stop.is_set():
            if self._line_quota is not None and self._local_emitted >= self._line_quota:
                break

            room = chunk
            if self._line_quota is not None:
                room = min(chunk, max(0, self._line_quota - self._local_emitted))
                if room <= 0:
                    break

            emitted = 0
            for _ in range(room):
                if self._line_quota is not None and self._local_emitted >= self._line_quota:
                    break
                n = self._emit_one_record()
                self.stats.add(n)
                self._local_emitted += n
                emitted += 1

            if emitted == 0:
                break

            next_t += emitted / rate
            sleep_t = next_t - time.perf_counter()
            if sleep_t > 0:
                time.sleep(sleep_t)
            elif sleep_t < -1.0:
                next_t = time.perf_counter()

        self._logger.info("worker stopped host=%s", self.host, extra=self._log_extra())

    def _run(self):
        last_burst = time.monotonic()
        w_rate = float(getattr(self.cfg, "benchmark_worker_lines_per_sec", 0.0) or 0.0)
        if w_rate > 0:
            self._run_rate_limited()
            return

        self._logger.info("worker started host=%s", self.host, extra=self._log_extra())

        while not self._stop.is_set():
            self._emit_cycle()
            pc = self.phase_config
            if pc.burst_size > 0 and time.monotonic() - last_burst >= pc.burst_interval:
                self._emit_burst()
                last_burst = time.monotonic()

        self._logger.info("worker stopped host=%s", self.host, extra=self._log_extra())


# ── ScenarioRunner ────────────────────────────────────────────────────────────

class ScenarioRunner:
    """
    Drives an AgentOfChaos instance through a sequence of phases, adjusting
    service counts and PhaseConfig settings between each one.

    Gradual transitions interpolate settings linearly over transition_sec,
    spreading service additions/removals evenly across that window so the
    agent sees a smooth ramp rather than an abrupt jump.
    """

    def __init__(self, scenario: dict, speed: float,
                 orchestrator: "AgentOfChaos", phase_config: PhaseConfig):
        self.phases       = scenario["phases"]
        self.loop         = scenario.get("loop", True)
        self.speed        = max(speed, 0.01)
        self.orchestrator = orchestrator
        self.phase_config = phase_config
        self._running     = True

    def stop(self):
        self._running = False

    def run(self):
        cycle = 0
        while self._running:
            cycle += 1
            label = f" (cycle {cycle})" if self.loop else ""
            self.orchestrator._event(f"[scenario] starting{label} — {len(self.phases)} phases")

            for phase in self.phases:
                if not self._running:
                    break
                self._execute_phase(phase)

            if not self.loop:
                self._running = False

        self.orchestrator._event("[scenario] sequence complete")

    # ── phase execution ───────────────────────────────────────────────────────

    def _execute_phase(self, phase: dict):
        name           = phase.get("name", "unnamed")
        raw_duration   = phase.get("duration", 60)
        duration       = raw_duration / self.speed
        raw_trans      = phase.get("transition_sec", 0)
        transition_sec = raw_trans / self.speed
        transition     = phase.get("transition", "immediate")
        target_svcs    = phase.get("services")

        # Collect only the settings this phase explicitly overrides
        override = {
            k: phase[k]
            for k in PhaseConfig._FIELDS
            if k in phase
        }

        eta = datetime.now().strftime("%H:%M:%S")
        svc_str = str(target_svcs) if target_svcs is not None else "~"
        self.orchestrator._set_phase(name)
        self.orchestrator._event(
            f"[phase:{name}]  duration={duration:.0f}s  services={svc_str}  "
            + "  ".join(f"{k}={v}" for k, v in override.items())
        )

        if transition == "gradual" and transition_sec > 0:
            t = threading.Thread(
                target=self._gradual_transition,
                args=(target_svcs, override, transition_sec),
                daemon=True,
            )
            t.start()
        else:
            if target_svcs is not None:
                self._apply_service_count(target_svcs)
            if override:
                self.phase_config.update(**override)

        # Wait out the phase duration, checking for stop every 0.5 s
        deadline = time.monotonic() + duration
        while self._running and time.monotonic() < deadline:
            time.sleep(0.5)

    # ── transitions ───────────────────────────────────────────────────────────

    def _gradual_transition(self, target_svcs: int | None,
                            target_settings: dict, transition_sec: float):
        """Smoothly ramp settings and service count over transition_sec."""
        start_snap    = self.phase_config.snapshot()
        start_svcs    = self.orchestrator.active_count()
        end_svcs      = target_svcs if target_svcs is not None else start_svcs

        steps    = max(2, int(transition_sec))   # one step per second
        interval = transition_sec / steps

        for i in range(1, steps + 1):
            if not self._running:
                break
            t = i / steps  # 0 → 1

            # Linearly interpolate each overridden numeric setting
            interpolated = {}
            for k, target_v in target_settings.items():
                start_v = start_snap.get(k, target_v)
                if isinstance(target_v, (int, float)) and isinstance(start_v, (int, float)):
                    raw = start_v + (target_v - start_v) * t
                    interpolated[k] = type(target_v)(round(raw) if isinstance(target_v, int) else raw)
            if interpolated:
                self.phase_config.update(**interpolated)

            # Spread service count changes across the window
            n = round(start_svcs + (end_svcs - start_svcs) * t)
            self._apply_service_count(n)

            time.sleep(interval)

        # Snap to exact targets at the end in case of rounding drift
        if target_settings:
            self.phase_config.update(**target_settings)
        if target_svcs is not None:
            self._apply_service_count(target_svcs)

    def _apply_service_count(self, target: int):
        current = self.orchestrator.active_count()
        if target > current:
            for svc in self.orchestrator.next_available_services(target - current):
                self.orchestrator._add_worker(svc)
        elif target < current:
            for svc in self.orchestrator.oldest_worker_names(current - target):
                self.orchestrator._remove_worker(svc)


# ── AgentOfChaos orchestrator ─────────────────────────────────────────────────

class AgentOfChaos:

    def __init__(self, cfg: argparse.Namespace):
        self.cfg          = cfg
        self.stats        = Stats()
        self.phase_config = PhaseConfig(cfg)
        self._running     = True
        self._lock        = threading.Lock()
        self._workers: dict[str, ServiceWorker] = {}
        self._pool        = list(SERVICE_POOL)
        if getattr(cfg, "deterministic", False):
            rng = random.Random(int(cfg.global_seed))
            rng.shuffle(self._pool)
        else:
            random.shuffle(self._pool)
        self._host_n      = 1
        self._current_phase = "—"

    # ── worker pool helpers ───────────────────────────────────────────────────

    def _next_host(self, service: str) -> str:
        h = f"{service[:8]}-{self._host_n:02d}"
        self._host_n += 1
        return h

    def next_available_services(self, n: int) -> list[str]:
        """Return up to n service names from the pool that aren't running."""
        with self._lock:
            active = set(self._workers.keys())
        return [s for s in self._pool if s not in active][:n]

    def oldest_worker_names(self, n: int) -> list[str]:
        """Return the n workers that have been running the longest."""
        with self._lock:
            by_age = sorted(self._workers.values(), key=lambda w: w.started_at)
        return [w.name for w in by_age[:n]]

    def active_count(self) -> int:
        with self._lock:
            return len(self._workers)

    # ── worker lifecycle ──────────────────────────────────────────────────────

    def _add_worker(self, name: str, line_quota: int | None = None):
        with self._lock:
            if name in self._workers:
                return
            w = ServiceWorker(
                name, self._next_host(name),
                self.cfg, self.phase_config, self.stats, line_quota,
            )
            w.start()
            self._workers[name] = w
        nf = 1 if getattr(self.cfg, "benchmark_plain_only", False) else 2
        self._event(f"[+] {name}  ({len(self._workers)} workers, "
                    f"{len(self._workers) * nf} files)")

    def _remove_worker(self, name: str):
        with self._lock:
            w = self._workers.pop(name, None)
        if w:
            w.stop()
            self._event(f"[-] {name}  ({len(self._workers)} workers)")

    # ── display ───────────────────────────────────────────────────────────────

    def _event(self, msg: str):
        ts = datetime.now().strftime("%H:%M:%S")
        print(f"  {ts}  {msg}")

    def _set_phase(self, name: str):
        self._current_phase = name

    def _stats_loop(self):
        ivl = self.cfg.stats_interval
        while self._running:
            time.sleep(ivl)
            if not self._running:
                break
            rate    = self.stats.rate_and_reset()
            total   = self.stats.total
            rots    = self.stats.rotations
            workers = self.active_count()
            pc      = self.phase_config
            ts      = datetime.now().strftime("%H:%M:%S")
            label = getattr(self.cfg, "benchmark_case_name", "") or "—"
            prof = getattr(self.cfg, "benchmark_datadog_profile", "") or ""
            prof_s = f"  profile={prof}" if prof else ""
            print(
                f"  {ts}  phase={self._current_phase:<18}  case={label:<28}{prof_s}  "
                f"workers={workers}  lpc={pc.lines_per_cycle}  "
                f"total={total:,}  rate={rate:,.0f}/s  rotations={rots}"
            )

    # ── modes ─────────────────────────────────────────────────────────────────

    def _run_steady(self):
        svcs = self._pool[: self.cfg.services]
        self._set_phase("steady")
        w_rate = float(getattr(self.cfg, "benchmark_worker_lines_per_sec", 0.0) or 0.0)
        bname = getattr(self.cfg, "benchmark_case_name", None)
        quotas: list[int | None] = [None] * len(svcs)
        if bname:
            prof = getattr(self.cfg, "benchmark_datadog_profile", "") or ""
            dur = getattr(self.cfg, "benchmark_duration_sec", None)
            cap = getattr(self.cfg, "benchmark_max_lines", None)
            tps = float(getattr(self.cfg, "benchmark_total_lines_per_sec", 0.0) or 0.0)
            lc = int(self.cfg.benchmark_line_cap)
            ech = max(1, int(getattr(self.cfg, "benchmark_emit_chunk", 32) or 1))
            print(f"  mode=steady (benchmark)  case={bname!r}  datadog_profile={prof!r}")
            print(f"  services={len(svcs)}  target={tps:,.3f} lines/s aggregate  "
                  f"per_worker={w_rate:,.4f} lines/s  emit_chunk={ech}")
            print(f"  deterministic_line_cap={lc:,}  (= min(duration_sec×rate, "
                  f"max_total_lines); split across workers for byte-identical re-runs)")
            print(f"  inputs:  duration_sec={dur!r}  max_total_lines={cap!r}")
            print(f"  deterministic={getattr(self.cfg, 'deterministic', False)}  "
                  f"global_seed={getattr(self.cfg, 'global_seed', 0)}  "
                  f"plain_only={getattr(self.cfg, 'benchmark_plain_only', False)}")
            quotas = _split_quota(lc, len(svcs))
        else:
            print(f"  mode=steady  services={len(svcs)}")
        for i, s in enumerate(svcs):
            self._add_worker(s, quotas[i])
        if bname:
            self._wait_benchmark_workers_idle()
        else:
            self._wait()

    def _wait_benchmark_workers_idle(self):
        self._event(
            f"[benchmark] waiting for workers (line_cap={int(self.cfg.benchmark_line_cap):,})",
        )
        while True:
            with self._lock:
                alive = [
                    w for w in self._workers.values()
                    if w._thread is not None and w._thread.is_alive()
                ]
            if not self._running:
                for w in list(self._workers.values()):
                    w.stop()
                break
            if not alive:
                break
            time.sleep(0.05)
        self._running = False

    def _run_ramp(self):
        target = self.cfg.services
        start  = min(self.cfg.ramp_start, target)
        step   = self.cfg.ramp_step
        ivl    = self.cfg.ramp_interval
        queue  = self._pool[:target]
        self._set_phase("ramp")
        print(f"  mode=ramp  {start} → {target} services  +{step} every {ivl}s")
        for s in queue[:start]:
            self._add_worker(s)
        idx = start
        while self._running and idx < len(queue):
            deadline = time.monotonic() + ivl
            while self._running and time.monotonic() < deadline:
                time.sleep(0.1)
            if not self._running:
                break
            for s in queue[idx: idx + step]:
                self._add_worker(s)
            idx += step
        if self._running:
            print(f"\n  Full ramp: {self.active_count()} workers, "
                  f"{self.active_count()*2} files")
            self._wait()

    def _run_chaos(self):
        svcs     = self._pool[: self.cfg.services]
        rate     = self.cfg.chaos_crash_rate
        delay    = self.cfg.chaos_restart_delay
        p_per_s  = rate / 60.0
        self._set_phase("chaos")
        print(f"  mode=chaos  services={len(svcs)}  "
              f"crash_rate={rate}/min  restart_delay={delay}s")
        for s in svcs:
            self._add_worker(s)
        while self._running:
            time.sleep(1)
            if not self._running:
                break
            with self._lock:
                candidates = list(self._workers.keys())
            for svc in candidates:
                if random.random() < p_per_s:
                    self._event(f"[chaos] crashing {svc} — restart in {delay}s")
                    self._remove_worker(svc)

                    def _restart(name=svc):
                        elapsed = 0.0
                        while elapsed < delay and self._running:
                            time.sleep(0.5)
                            elapsed += 0.5
                        if self._running:
                            self._add_worker(name)

                    threading.Thread(target=_restart, daemon=True).start()

    def _run_spike(self):
        base   = self._pool[: self.cfg.services]
        extra  = [s for s in SERVICE_POOL if s not in base][: self.cfg.spike_services]
        ivl    = self.cfg.spike_interval
        dur    = self.cfg.spike_duration
        self._set_phase("spike-baseline")
        print(f"  mode=spike  baseline={len(base)}  "
              f"+{len(extra)} every {ivl}s for {dur}s")
        for s in base:
            self._add_worker(s)
        while self._running:
            deadline = time.monotonic() + ivl
            while self._running and time.monotonic() < deadline:
                time.sleep(0.1)
            if not self._running:
                break
            self._set_phase("spike-active")
            self._event(f"[spike] START +{len(extra)} workers for {dur}s")
            for s in extra:
                self._add_worker(s)

            def _end(svcs=list(extra)):
                elapsed = 0.0
                while elapsed < dur and self._running:
                    time.sleep(0.5)
                    elapsed += 0.5
                for s in svcs:
                    if self._running:
                        self._remove_worker(s)
                self._set_phase("spike-baseline")
                self._event(f"[spike] END  back to {self.active_count()} workers")

            threading.Thread(target=_end, daemon=True).start()

    def _run_scenario(self, scenario: dict):
        speed  = self.cfg.scenario_speed
        runner = ScenarioRunner(scenario, speed, self, self.phase_config)
        threading.Thread(target=runner.run, name="scenario-runner", daemon=True).start()
        self._wait()
        runner.stop()

    def _build_pulse_scenario(self) -> dict:
        cfg      = self.cfg
        ramp_s   = cfg.pulse_ramp
        peak_s   = cfg.pulse_peak
        cool_s   = cfg.pulse_cooldown if cfg.pulse_cooldown > 0 else cfg.pulse_ramp
        rest_s   = cfg.pulse_rest
        n        = cfg.pulse_cycles   # 0 = loop forever

        base = {
            "services":        cfg.pulse_base_services,
            "lines_per_cycle": cfg.pulse_base_lines,
            "burst_size":      cfg.burst_size,
            "burst_interval":  cfg.burst_interval,
            "cycle_sleep_ms":  cfg.cycle_sleep_ms,
        }
        peak = {
            "services":        cfg.pulse_peak_services,
            "lines_per_cycle": cfg.pulse_peak_lines,
            "burst_size":      cfg.pulse_peak_burst_size,
            "burst_interval":  cfg.pulse_peak_burst_interval,
            "cycle_sleep_ms":  0,
        }

        # One snap-to-base phase at the very start so the first ramp always
        # begins from a known baseline (even on cycle 1 when workers = 0).
        init_phase = {"name": "base", "duration": 2, **base}

        core_phases = [
            {
                "name": "ramp-up",
                "duration": ramp_s,
                "transition": "gradual",
                "transition_sec": ramp_s,
                **peak,
            },
            {
                "name": "peak",
                "duration": peak_s,
                **peak,
            },
            {
                "name": "cooldown",
                "duration": cool_s,
                "transition": "gradual",
                "transition_sec": cool_s,
                **base,
            },
            {
                "name": "rest",
                "duration": rest_s,
                **base,
            },
        ]

        if n == 0:
            return {"loop": True,  "phases": [init_phase] + core_phases}
        else:
            # Pre-expand N cycles; init_phase runs once, core repeats N times.
            return {"loop": False, "phases": [init_phase] + core_phases * n}

    def _run_pulse(self):
        cfg    = self.cfg
        ramp_s = cfg.pulse_ramp
        peak_s = cfg.pulse_peak
        cool_s = cfg.pulse_cooldown if cfg.pulse_cooldown > 0 else cfg.pulse_ramp
        rest_s = cfg.pulse_rest
        cycle  = ramp_s + peak_s + cool_s + rest_s
        cycles = "∞" if cfg.pulse_cycles == 0 else str(cfg.pulse_cycles)
        speed  = cfg.scenario_speed

        print(f"  mode=pulse  cycles={cycles}  "
              f"cycle_time={cycle//60:.0f}m{cycle%60:02.0f}s"
              + (f"  (speed={speed}x → {cycle/speed//60:.0f}m{cycle/speed%60:02.0f}s real)"
                 if speed != 1.0 else ""))
        print(f"  base  → {cfg.pulse_base_services} services  "
              f"{cfg.pulse_base_lines} lines/cycle  "
              f"burst={cfg.burst_size} every {cfg.burst_interval}s")
        print(f"  peak  → {cfg.pulse_peak_services} services  "
              f"{cfg.pulse_peak_lines} lines/cycle  "
              f"burst={cfg.pulse_peak_burst_size} every {cfg.pulse_peak_burst_interval}s")
        print(f"  ramp-up={ramp_s}s  hold={peak_s}s  "
              f"cooldown={cool_s}s  rest={rest_s}s")

        self._run_scenario(self._build_pulse_scenario())

    # ── run / shutdown ────────────────────────────────────────────────────────

    def _wait(self):
        while self._running:
            time.sleep(0.1)

    def run(self):
        signal.signal(signal.SIGINT,  self._on_signal)
        signal.signal(signal.SIGTERM, self._on_signal)
        os.makedirs(self.cfg.log_dir, exist_ok=True)

        print(f"\nagent_of_chaos")
        print(f"  log_dir={self.cfg.log_dir}")
        if getattr(self.cfg, "benchmark_case_name", None):
            print(f"  benchmark_case={self.cfg.benchmark_case_name!r}")
        wl = float(getattr(self.cfg, "wide_line_rate", 0.03))
        po = bool(getattr(self.cfg, "benchmark_plain_only", False))
        print(f"  rotation={self.cfg.max_bytes//1024}KB x {self.cfg.backup_count}  "
              f"multiline_rate={self.cfg.multiline_rate:.0%}  "
              f"wide_lines={self.cfg.wide_lines}  wide_line_rate={wl:.0%}  "
              f"plain_only={po}")

        threading.Thread(target=self._stats_loop, daemon=True).start()

        try:
            mode = self.cfg.mode
            if mode == "steady":
                self._run_steady()
            elif mode == "ramp":
                self._run_ramp()
            elif mode == "chaos":
                self._run_chaos()
            elif mode == "spike":
                self._run_spike()
            elif mode == "pulse":
                self._run_pulse()
            elif mode == "scenario":
                scenario = self._load_scenario()
                if scenario:
                    desc = scenario.get("description", "")
                    loop = scenario.get("loop", True)
                    speed = self.cfg.scenario_speed
                    print(f"  scenario={self.cfg.scenario}  speed={speed}x  "
                          f"loop={loop}  phases={len(scenario['phases'])}")
                    if desc:
                        print(f"  {desc}")
                    self._run_scenario(scenario)
        finally:
            self.shutdown()

    def _load_scenario(self) -> dict | None:
        name = self.cfg.scenario
        path = self.cfg.scenario_file

        if path:
            try:
                with open(path) as f:
                    return json.load(f)
            except Exception as e:
                print(f"error: cannot load scenario file {path!r}: {e}", file=sys.stderr)
                return None

        if name:
            if name not in BUILTIN_SCENARIOS:
                print(f"error: unknown scenario {name!r}. "
                      f"Available: {', '.join(BUILTIN_SCENARIOS)}", file=sys.stderr)
                return None
            return BUILTIN_SCENARIOS[name]

        print("error: --mode scenario requires --scenario NAME or --scenario-file PATH",
              file=sys.stderr)
        return None

    def shutdown(self):
        self._running = False
        print("\n  Shutting down workers...")
        with self._lock:
            workers = list(self._workers.values())
        for w in workers:
            w.stop()
        logging.shutdown()
        print(f"  Done.  {self.stats.total:,} lines  "
              f"{self.stats.rotations} rotations  "
              f"files in: {self.cfg.log_dir}\n")

    def _on_signal(self, signum, frame):
        self._running = False


# ── JSON benchmark suite (deterministic, steady rate) ───────────────────────

_BENCHMARK_JSON_ALIASES = {
    "seed": "global_seed",
    "duration_sec": "benchmark_duration_sec",
    "max_total_lines": "benchmark_max_lines",
    "total_lines_per_sec": "benchmark_total_lines_per_sec",
    "datadog_profile": "benchmark_datadog_profile",
    "profile": "benchmark_datadog_profile",
    "name": "benchmark_case_name",
    "log_subdir": "benchmark_log_subdir",
    "plain_only": "benchmark_plain_only",
}


def _flatten_benchmark_keys(d: dict) -> dict:
    out: dict = {}
    for k, v in d.items():
        out[_BENCHMARK_JSON_ALIASES.get(k, k)] = v
    return out


def _coerce_benchmark_types(m: dict) -> None:
    if m.get("services") is not None:
        m["services"] = int(m["services"])
    if m.get("benchmark_max_lines") is not None:
        m["benchmark_max_lines"] = int(m["benchmark_max_lines"])
    if m.get("benchmark_duration_sec") is not None:
        m["benchmark_duration_sec"] = int(m["benchmark_duration_sec"])
    if m.get("benchmark_total_lines_per_sec") is not None:
        m["benchmark_total_lines_per_sec"] = float(m["benchmark_total_lines_per_sec"])
    if m.get("global_seed") is not None:
        m["global_seed"] = int(m["global_seed"])
    if m.get("lines_per_cycle") is not None:
        m["lines_per_cycle"] = int(m["lines_per_cycle"])
    if m.get("burst_size") is not None:
        m["burst_size"] = int(m["burst_size"])
    if m.get("burst_interval") is not None:
        m["burst_interval"] = float(m["burst_interval"])
    if m.get("cycle_sleep_ms") is not None:
        m["cycle_sleep_ms"] = int(m["cycle_sleep_ms"])
    if m.get("multiline_rate") is not None:
        m["multiline_rate"] = float(m["multiline_rate"])
    if m.get("max_bytes") is not None:
        m["max_bytes"] = int(m["max_bytes"])
    if m.get("backup_count") is not None:
        m["backup_count"] = int(m["backup_count"])
    if m.get("benchmark_emit_chunk") is not None:
        m["benchmark_emit_chunk"] = int(m["benchmark_emit_chunk"])
    if "wide_lines" in m:
        m["wide_lines"] = bool(m["wide_lines"])
    if m.get("wide_line_rate") is not None:
        m["wide_line_rate"] = float(m["wide_line_rate"])
    if "deterministic" in m:
        m["deterministic"] = bool(m["deterministic"])
    if "benchmark_plain_only" in m:
        m["benchmark_plain_only"] = bool(m["benchmark_plain_only"])


def load_benchmark_document(path: str) -> dict:
    with open(path, encoding="utf-8") as f:
        doc = json.load(f)
    if "benchmarks" not in doc or not isinstance(doc["benchmarks"], list):
        raise ValueError("benchmark JSON must contain a 'benchmarks' array")
    return doc


def merge_benchmark_case(defaults: dict | None, case: dict) -> dict:
    merged: dict = {}
    if defaults:
        merged.update(_flatten_benchmark_keys(defaults))
    merged.update(_flatten_benchmark_keys(case))
    _coerce_benchmark_types(merged)
    return merged


def _validate_benchmark_case(m: dict, idx: int) -> None:
    name = m.get("benchmark_case_name")
    if not name:
        raise ValueError(f"benchmark #{idx} missing 'name'")
    dur = m.get("benchmark_duration_sec")
    cap = m.get("benchmark_max_lines")
    if dur is None and cap is None:
        raise ValueError(
            f"benchmark {name!r}: set at least one of duration_sec, max_total_lines",
        )
    tps = m.get("benchmark_total_lines_per_sec")
    if tps is None or float(tps) <= 0:
        raise ValueError(f"benchmark {name!r}: total_lines_per_sec must be > 0")


def _benchmark_line_cap(cfg: argparse.Namespace) -> int:
    """Deterministic total line budget: min of explicit cap and duration×rate (floor)."""
    tps = float(cfg.benchmark_total_lines_per_sec)
    parts: list[int] = []
    dur = getattr(cfg, "benchmark_duration_sec", None)
    cap = getattr(cfg, "benchmark_max_lines", None)
    if dur is not None:
        parts.append(int(float(dur) * tps))
    if cap is not None:
        parts.append(int(cap))
    if not parts:
        raise ValueError("internal: line cap parts empty")
    return min(parts)


def _split_quota(total: int, n: int) -> list[int]:
    if n <= 0:
        return []
    base, rem = divmod(int(total), int(n))
    return [base + (1 if i < rem else 0) for i in range(n)]


def build_cfg_for_benchmark(base: argparse.Namespace, merged: dict) -> argparse.Namespace:
    cfg = copy.copy(base)
    had_det_key = "deterministic" in merged
    for k, v in merged.items():
        setattr(cfg, k, v)
    if not had_det_key:
        cfg.deterministic = True
    n = int(cfg.services)
    tps = float(cfg.benchmark_total_lines_per_sec)
    cfg.benchmark_worker_lines_per_sec = (tps / n) if n > 0 and tps > 0 else 0.0
    cfg.mode = "steady"
    cfg.benchmark_line_cap = _benchmark_line_cap(cfg)
    return cfg


def run_benchmark_list(path: str) -> None:
    doc = load_benchmark_document(path)
    defaults = doc.get("defaults") or {}
    print(f"\nBenchmark cases in {path!r}:\n")
    for i, raw in enumerate(doc["benchmarks"]):
        m = merge_benchmark_case(defaults, raw)
        nm = m.get("benchmark_case_name", "?")
        prof = m.get("benchmark_datadog_profile", "")
        print(f"  {nm}")
        if prof:
            print(f"    datadog_profile (for your agent yaml): {prof}")
        print(
            f"    duration_sec={m.get('benchmark_duration_sec')!r}  "
            f"max_total_lines={m.get('benchmark_max_lines')!r}  "
            f"total_lines_per_sec={m.get('benchmark_total_lines_per_sec')!r}  "
            f"services={m.get('services')!r}  seed={m.get('global_seed')!r}",
        )
        print()


def run_benchmark_suite(argv_cfg: argparse.Namespace) -> None:
    path = argv_cfg.benchmark_config
    assert path
    doc = load_benchmark_document(path)
    defaults = doc.get("defaults") or {}
    if argv_cfg.run_all_benchmarks:
        cases = doc["benchmarks"]
    else:
        target = argv_cfg.benchmark
        if not target:
            print("error: use --benchmark NAME or --run-all-benchmarks", file=sys.stderr)
            sys.exit(2)
        cases = [c for c in doc["benchmarks"] if c.get("name") == target]
        if not cases:
            print(f"error: no benchmark named {target!r} in {path!r}", file=sys.stderr)
            sys.exit(2)

    for i, raw in enumerate(cases):
        merged = merge_benchmark_case(defaults, raw)
        _validate_benchmark_case(merged, i)
        cfg = build_cfg_for_benchmark(argv_cfg, merged)
        sub = getattr(cfg, "benchmark_log_subdir", None)
        if sub:
            cfg.log_dir = os.path.join(cfg.log_dir, str(sub))
        print(f"\n{'=' * 72}\n"
              f"  Benchmark {i + 1}/{len(cases)}  {cfg.benchmark_case_name!r}\n"
              f"{'=' * 72}")
        AgentOfChaos(cfg).run()


# ── CLI ───────────────────────────────────────────────────────────────────────

def _list_scenarios():
    print("\nBuilt-in scenarios:\n")
    for name, sc in BUILTIN_SCENARIOS.items():
        phases = sc.get("phases", [])
        loop   = sc.get("loop", False)
        desc   = sc.get("description", "")
        total  = sum(p.get("duration", 0) for p in phases)
        print(f"  {name}")
        print(f"    {desc}")
        print(f"    phases={len(phases)}  total_duration={total//60:.0f}min  loop={loop}")
        print()
    print("Use --scenario-file to load a custom JSON scenario.")
    print("Run with --scenario NAME --scenario-speed N to compress durations.\n")


# ── Environment variable defaults (AOCH_*; CLI overrides) ───────────────────────

AOCH_ENV_PREFIX = "AOCH_"


def _env_str(key: str, default: str | None = None) -> str | None:
    val = os.environ.get(f"{AOCH_ENV_PREFIX}{key}")
    if val is None or val == "":
        return default
    return val


def _env_int(key: str, default: int) -> int:
    val = os.environ.get(f"{AOCH_ENV_PREFIX}{key}")
    if val is None or val == "":
        return default
    return int(val)


def _env_float(key: str, default: float) -> float:
    val = os.environ.get(f"{AOCH_ENV_PREFIX}{key}")
    if val is None or val == "":
        return default
    return float(val)


def _env_bool(key: str, default: bool = False) -> bool:
    val = os.environ.get(f"{AOCH_ENV_PREFIX}{key}")
    if val is None or val == "":
        return default
    return val.strip().lower() in ("1", "true", "yes", "on")


def _apply_benchmark_namespace_defaults(cfg: argparse.Namespace) -> None:
    for k, v in (
        ("benchmark_worker_lines_per_sec", 0.0),
        ("benchmark_case_name", None),
        ("benchmark_datadog_profile", None),
        ("benchmark_duration_sec", None),
        ("benchmark_max_lines", None),
        ("benchmark_total_lines_per_sec", None),
        ("benchmark_log_subdir", None),
        ("benchmark_line_cap", None),
    ):
        if not hasattr(cfg, k):
            setattr(cfg, k, v)


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(
        prog="agent_of_chaos",
        description="Datadog Logs Agent stress tester",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )

    p.add_argument(
        "--mode",
        choices=["steady", "ramp", "chaos", "spike", "scenario", "pulse"],
        default=_env_str("MODE", "steady"),
        help="steady | ramp | chaos | spike | scenario | pulse (env: AOCH_MODE)",
    )

    # ── volume ────────────────────────────────────────────────────────────────
    vol = p.add_argument_group("volume")
    vol.add_argument("--services", type=int, default=_env_int("SERVICES", 5),
                     help="service workers (ignored in scenario mode)")
    vol.add_argument("--lines-per-cycle", type=int,
                     default=_env_int("LINES_PER_CYCLE", 20),
                     help="lines per worker per tight-loop cycle")
    vol.add_argument("--cycle-sleep-ms", type=int,
                     default=_env_int("CYCLE_SLEEP_MS", 0),
                     help="ms to sleep between worker cycles (0 = flat-out)")
    vol.add_argument("--burst-interval", type=float,
                     default=_env_float("BURST_INTERVAL", 5.0),
                     help="seconds between per-service burst events")
    vol.add_argument("--burst-size", type=int, default=_env_int("BURST_SIZE", 1000),
                     help="lines per burst event")

    # ── rotation ──────────────────────────────────────────────────────────────
    rot = p.add_argument_group("rotation")
    rot.add_argument("--max-bytes", type=int,
                     default=_env_int("MAX_BYTES", 512 * 1024),
                     help="file size before rotation")
    rot.add_argument("--backup-count", type=int, default=_env_int("BACKUP_COUNT", 5),
                     help="rotated backup files to keep per log")

    # ── ramp mode ─────────────────────────────────────────────────────────────
    ramp = p.add_argument_group("ramp mode")
    ramp.add_argument("--ramp-start", type=int, default=_env_int("RAMP_START", 1))
    ramp.add_argument("--ramp-step",  type=int, default=_env_int("RAMP_STEP", 1))
    ramp.add_argument("--ramp-interval", type=int, default=_env_int("RAMP_INTERVAL", 30),
                      help="seconds between ramp steps")

    # ── chaos mode ────────────────────────────────────────────────────────────
    chaos = p.add_argument_group("chaos mode")
    chaos.add_argument("--chaos-crash-rate", type=float,
                       default=_env_float("CHAOS_CRASH_RATE", 2.0),
                       help="expected crashes per service per minute")
    chaos.add_argument("--chaos-restart-delay", type=int,
                       default=_env_int("CHAOS_RESTART_DELAY", 5),
                       help="seconds before a crashed service restarts")

    # ── spike mode ────────────────────────────────────────────────────────────
    spike = p.add_argument_group("spike mode")
    spike.add_argument("--spike-interval", type=int,
                       default=_env_int("SPIKE_INTERVAL", 30))
    spike.add_argument("--spike-duration", type=int,
                       default=_env_int("SPIKE_DURATION", 10))
    spike.add_argument("--spike-services", type=int,
                       default=_env_int("SPIKE_SERVICES", 5))

    # ── pulse mode ────────────────────────────────────────────────────────────
    pulse = p.add_argument_group(
        "pulse mode",
        "Repeating pressure-test cycle: ramp-up → peak → cooldown → rest → repeat.\n"
        "Defaults match the described pattern: 10 min ramp, 3 min peak, 35 min rest.",
    )
    pulse.add_argument("--pulse-ramp", type=int, default=_env_int("PULSE_RAMP", 600),
                       help="ramp-up duration in seconds")
    pulse.add_argument("--pulse-peak", type=int, default=_env_int("PULSE_PEAK", 180),
                       help="sustained peak duration in seconds")
    pulse.add_argument("--pulse-cooldown", type=int,
                       default=_env_int("PULSE_COOLDOWN", 0),
                       help="cooldown duration in seconds (0 = same as --pulse-ramp)")
    pulse.add_argument("--pulse-rest", type=int, default=_env_int("PULSE_REST", 2100),
                       help="quiet rest period in seconds")
    pulse.add_argument("--pulse-base-services", type=int,
                       default=_env_int("PULSE_BASE_SERVICES", 2),
                       help="service count at baseline")
    pulse.add_argument("--pulse-peak-services", type=int,
                       default=_env_int("PULSE_PEAK_SERVICES", 10),
                       help="service count at peak")
    pulse.add_argument("--pulse-base-lines", type=int,
                       default=_env_int("PULSE_BASE_LINES", 4),
                       help="lines per cycle at baseline")
    pulse.add_argument("--pulse-peak-lines", type=int,
                       default=_env_int("PULSE_PEAK_LINES", 40),
                       help="lines per cycle at peak")
    pulse.add_argument("--pulse-peak-burst-size", type=int,
                       default=_env_int("PULSE_PEAK_BURST_SIZE", 2000),
                       help="burst size at peak")
    pulse.add_argument("--pulse-peak-burst-interval", type=float,
                       default=_env_float("PULSE_PEAK_BURST_INTERVAL", 4.0),
                       help="seconds between bursts at peak")
    pulse.add_argument("--pulse-cycles", type=int, default=_env_int("PULSE_CYCLES", 0),
                       help="number of pulse cycles to run (0 = loop forever)")

    # ── scenario mode ─────────────────────────────────────────────────────────
    sc = p.add_argument_group("scenario mode")
    sc.add_argument(
        "--scenario",
        metavar="NAME",
        default=_env_str("SCENARIO"),
        help=(
            "built-in scenario to run: "
            + ", ".join(BUILTIN_SCENARIOS)
            + " — or 'list' to show descriptions"
        ),
    )
    sc.add_argument("--scenario-file", metavar="PATH",
                    default=_env_str("SCENARIO_FILE"),
                    help="path to a custom JSON scenario file")
    sc.add_argument("--scenario-speed", type=float,
                    default=_env_float("SCENARIO_SPEED", 1.0),
                    help="divide all phase durations by this factor "
                         "(e.g. 6 compresses a 6-hour scenario into 1 hour)")

    # ── content ───────────────────────────────────────────────────────────────
    content = p.add_argument_group("content")
    content.add_argument("--multiline-rate", type=float,
                         default=_env_float("MULTILINE_RATE", 0.20),
                         help="fraction of lines that emit a stack trace")
    content.add_argument("--wide-lines", action="store_true",
                         default=_env_bool("WIDE_LINES"),
                         help="emit very long WIDE-LINE rows (see --wide-line-rate)")
    content.add_argument(
        "--wide-line-rate",
        type=float,
        default=_env_float("WIDE_LINE_RATE", 0.03),
        metavar="P",
        help="with --wide-lines, probability each record pass emits a WIDE-LINE",
    )

    det = p.add_argument_group(
        "deterministic logs (CLI steady mode)",
        "Per-worker RNG derived from --global-seed and synthetic timestamps "
        "so log bytes repeat across runs when nothing else changes.",
    )
    det.add_argument(
        "--deterministic",
        action="store_true",
        default=_env_bool("DETERMINISTIC"),
        help="use seeded RNG + synthetic timestamps (pair with --global-seed)",
    )
    det.add_argument(
        "--global-seed",
        type=int,
        default=_env_int("GLOBAL_SEED", 0),
        metavar="N",
        help="seed base for --deterministic (same as JSON 'seed')",
    )

    bench = p.add_argument_group(
        "JSON benchmark suite",
        "Load defaults + named cases; steady aggregate rate; stop when first of "
        "duration_sec or max_total_lines. See benchmark_profiles.json.",
    )
    bench.add_argument(
        "--benchmark-config",
        metavar="PATH",
        default=_env_str("BENCHMARK_CONFIG"),
        help="JSON file with 'defaults' and 'benchmarks' array",
    )
    bench.add_argument(
        "--benchmark",
        metavar="NAME",
        default=_env_str("BENCHMARK"),
        help="run a single case's 'name' from the JSON file",
    )
    bench.add_argument(
        "--run-all-benchmarks",
        action="store_true",
        default=_env_bool("RUN_ALL_BENCHMARKS"),
        help="run every case in benchmarks[] in order",
    )
    bench.add_argument(
        "--benchmark-list",
        action="store_true",
        default=_env_bool("BENCHMARK_LIST"),
        help="list benchmark names from --benchmark-config and exit",
    )
    bench.add_argument(
        "--benchmark-emit-chunk",
        type=int,
        default=_env_int("BENCHMARK_EMIT_CHUNK", 32),
        metavar="N",
        help="benchmark rate mode: emit N primary records per sleep slice (higher → "
             "closer to target lines/s; JSON key benchmark_emit_chunk overrides)",
    )
    bench.add_argument(
        "--benchmark-plain-only",
        action="store_true",
        default=_env_bool("BENCHMARK_PLAIN_ONLY"),
        help="benchmark: write only *.log (no *.json.log); JSON key benchmark_plain_only "
             "or plain_only",
    )

    # ── output ────────────────────────────────────────────────────────────────
    out = p.add_argument_group("output")
    out.add_argument(
        "--log-dir",
        default=_env_str("LOG_DIR", DEFAULT_LOG_DIR),
        metavar="DIR",
        help="where to write *.log / *.json.log (default: ./logs next to this script)",
    )
    out.add_argument("--stats-interval", type=int,
                     default=_env_int("STATS_INTERVAL", 5),
                     help="seconds between live stats lines")

    cfg = p.parse_args()
    _apply_benchmark_namespace_defaults(cfg)
    return cfg


def main():
    cfg = parse_args()

    # Special: list scenarios and exit
    if cfg.scenario == "list":
        _list_scenarios()
        sys.exit(0)

    if cfg.benchmark_config:
        if cfg.benchmark_list:
            run_benchmark_list(cfg.benchmark_config)
            sys.exit(0)
        run_benchmark_suite(cfg)
        sys.exit(0)

    AgentOfChaos(cfg).run()


if __name__ == "__main__":
    main()
