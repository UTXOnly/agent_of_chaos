package gen

import (
	"math/rand/v2"
	"strconv"
)

// ServicePool is the ordered list of stream names handed out to workers. Names
// beyond the pool length get a numeric suffix (auth-service-2, ...).
var ServicePool = []string{
	"auth-service", "payment-service", "api-gateway", "worker", "cache-proxy",
	"notification-svc", "db-proxy", "metrics-collector", "event-bus", "scheduler",
	"search-indexer", "cdn-router", "session-manager", "audit-logger", "config-svc",
	"rate-limiter", "image-processor", "email-sender", "webhook-dispatcher",
	"data-pipeline", "user-service", "order-service", "inventory-service",
	"reporting-service",
}

var users = []string{
	"user_1042", "user_8871", "user_3305", "user_9912", "anonymous",
	"user_0071", "user_4420", "user_6634", "svc-account", "bot_crawler",
}

var endpoints = []string{
	"/api/v1/login", "/api/v1/orders", "/api/v1/checkout", "/healthz", "/metrics",
	"/api/v2/search", "/api/v1/profile", "/api/v1/notifications", "/admin/status",
	"/api/v1/cart",
}

var httpStatuses = []string{"200", "201", "204", "301", "400", "401", "403", "404", "429", "500", "502", "503"}

// Level is a log severity. The numeric order matches the Python original.
type Level uint8

const (
	Debug Level = iota
	Info
	Warn
	Error
	Critical
)

// Padded to 8 columns like the Python `%(levelname)-8s` format so plain-text
// columns line up.
var levelNames = [...]string{"DEBUG   ", "INFO    ", "WARNING ", "ERROR   ", "CRITICAL"}
var levelNamesTrim = [...]string{"DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"}

// levelWeights mirrors the Python LEVEL_WEIGHTS list: 2 debug, 3 info, 1 each
// of warning/error/critical.
var levelWeights = [...]Level{Debug, Debug, Info, Info, Info, Warn, Error, Critical}

func (l Level) String() string { return levelNamesTrim[l] }

// placeholder kinds inside a message template.
type ph uint8

const (
	phLiteral  ph = iota
	phKey         // sess:NNNN
	phResult      // HIT | MISS
	phMs          // 1..2000
	phUser        // from users
	phReq         // req-NNNN
	phInstance    // i-1NN
	phService     // stream name
	phN           // 1..9
	phVal         // true | false
	phBytes       // 64..65536
	phEndpoint    // from endpoints
	phStatus      // http status
	phPatch       // 0..19
	phPct         // 70..99
)

var phByName = map[string]ph{
	"key": phKey, "result": phResult, "ms": phMs, "user": phUser, "req": phReq,
	"instance": phInstance, "service": phService, "n": phN, "val": phVal,
	"bytes": phBytes, "endpoint": phEndpoint, "status": phStatus,
	"patch": phPatch, "pct": phPct,
}

type segment struct {
	kind ph
	lit  string
}

type template []segment

func compile(s string) template {
	var t template
	for len(s) > 0 {
		i := indexByte(s, '{')
		if i < 0 {
			t = append(t, segment{kind: phLiteral, lit: s})
			break
		}
		j := indexByte(s[i:], '}')
		if j < 0 {
			t = append(t, segment{kind: phLiteral, lit: s})
			break
		}
		if i > 0 {
			t = append(t, segment{kind: phLiteral, lit: s[:i]})
		}
		name := s[i+1 : i+j]
		k, ok := phByName[name]
		if !ok {
			panic("gen: unknown placeholder {" + name + "}")
		}
		t = append(t, segment{kind: k})
		s = s[i+j+1:]
	}
	return t
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func compileAll(src []string) []template {
	out := make([]template, len(src))
	for i, s := range src {
		out[i] = compile(s)
	}
	return out
}

var messages = [5][]template{
	Debug: compileAll([]string{
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
	}),
	Info: compileAll([]string{
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
	}),
	Warn: compileAll([]string{
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
	}),
	Error: compileAll([]string{
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
	}),
	Critical: compileAll([]string{
		"CRITICAL: Database primary node is unreachable — all writes failing",
		"CRITICAL: Out of memory — service {service} is being terminated",
		"CRITICAL: SSL certificate for {endpoint} expired",
		"CRITICAL: Message queue backlog exceeded 10,000 — dropping messages",
		"CRITICAL: Security alert — brute force detected from 192.168.1.{n}",
		"CRITICAL: Disk at {pct}% capacity — log rotation blocked",
		"CRITICAL: Consensus lost — {service} split-brain detected",
	}),
}

// appendTemplate renders t into buf using rng for every placeholder.
func appendTemplate(buf []byte, t template, service string, rng *rand.Rand) []byte {
	for _, seg := range t {
		switch seg.kind {
		case phLiteral:
			buf = append(buf, seg.lit...)
		case phKey:
			buf = append(buf, "sess:"...)
			buf = strconv.AppendInt(buf, int64(1000+rng.IntN(9000)), 10)
		case phResult:
			if rng.IntN(2) == 0 {
				buf = append(buf, "HIT"...)
			} else {
				buf = append(buf, "MISS"...)
			}
		case phMs:
			buf = strconv.AppendInt(buf, int64(1+rng.IntN(2000)), 10)
		case phUser:
			buf = append(buf, users[rng.IntN(len(users))]...)
		case phReq:
			buf = append(buf, "req-"...)
			buf = appendPadInt(buf, 1+rng.IntN(199), 4)
		case phInstance:
			buf = append(buf, "i-"...)
			buf = strconv.AppendInt(buf, int64(100+rng.IntN(100)), 10)
		case phService:
			buf = append(buf, service...)
		case phN:
			buf = strconv.AppendInt(buf, int64(1+rng.IntN(9)), 10)
		case phVal:
			if rng.IntN(2) == 0 {
				buf = append(buf, "true"...)
			} else {
				buf = append(buf, "false"...)
			}
		case phBytes:
			buf = strconv.AppendInt(buf, int64(64+rng.IntN(65536-64+1)), 10)
		case phEndpoint:
			buf = append(buf, endpoints[rng.IntN(len(endpoints))]...)
		case phStatus:
			buf = append(buf, httpStatuses[rng.IntN(len(httpStatuses))]...)
		case phPatch:
			buf = strconv.AppendInt(buf, int64(rng.IntN(20)), 10)
		case phPct:
			buf = strconv.AppendInt(buf, int64(70+rng.IntN(30)), 10)
		}
	}
	return buf
}

func appendPadInt(buf []byte, n, width int) []byte {
	var tmp [20]byte
	s := strconv.AppendInt(tmp[:0], int64(n), 10)
	for i := len(s); i < width; i++ {
		buf = append(buf, '0')
	}
	return append(buf, s...)
}

// ── stack traces ─────────────────────────────────────────────────────────────

type exception struct{ kind, msg string }

var pyExceptions = []exception{
	{"ValueError", "Invalid value for field 'amount': expected float, got 'NaN'"},
	{"KeyError", "'session_token'"},
	{"ConnectionRefusedError", "[Errno 111] Connection refused"},
	{"TimeoutError", "Request to payment gateway timed out after 30s"},
	{"PermissionError", "[Errno 13] Permission denied: '/var/run/agent.sock'"},
	{"RuntimeError", "Worker pool exhausted — all 16 workers busy"},
	{"AttributeError", "'NoneType' object has no attribute 'user_id'"},
	{"IndexError", "list index out of range"},
	{"OSError", "[Errno 28] No space left on device"},
	{"RecursionError", "maximum recursion depth exceeded"},
}

var javaExceptions = []exception{
	{"java.lang.IllegalStateException", "Connection pool exhausted (max=32)"},
	{"java.net.SocketTimeoutException", "Read timed out"},
	{"java.lang.NullPointerException", "Cannot invoke \"User.getId()\" because \"user\" is null"},
	{"org.springframework.dao.DataAccessResourceFailureException", "Could not open JDBC Connection"},
	{"com.fasterxml.jackson.core.JsonParseException", "Unexpected character ('}' (code 125))"},
	{"java.util.concurrent.TimeoutException", "Future timed out after 5000 ms"},
}

var goPanics = []string{
	"runtime error: invalid memory address or nil pointer dereference",
	"runtime error: index out of range [5] with length 3",
	"send on closed channel",
	"sync: negative WaitGroup counter",
	"context deadline exceeded",
}

var pyFuncs = []string{"handle_request", "_process", "dispatch", "fetch_user", "commit", "run_job", "serialize", "validate"}
var pyLibs = []string{"requests/adapters", "sqlalchemy/engine/base", "urllib3/connectionpool", "redis/connection", "kafka/producer", "asyncio/base_events"}
var pyCode = []string{
	"result = self._process(request)",
	"conn = pool.acquire(timeout=self.timeout)",
	"return json.loads(payload)[\"session_token\"]",
	"raise TimeoutError(f\"Request to {target} timed out after {t}s\")",
	"row = cursor.fetchone()",
	"self.sock.sendall(frame)",
}
var javaClasses = []string{"OrderService", "PaymentClient", "SessionRepository", "RequestHandler", "KafkaProducerWrapper", "JdbcTemplate"}
var javaMethods = []string{"execute", "handle", "process", "doFilter", "invoke", "commit", "fetch", "lambda$run$0"}
var goFuncs = []string{"(*Server).handle", "(*Client).Do", "(*pool).acquire", "worker.run", "(*Decoder).Decode", "processBatch"}

const stackTraceKinds = 3

// appendStackTrace writes a multi-line stack trace (without a trailing newline
// on the last line) in one of three flavours. Each physical line is separated
// by '\n'. Callers embedding this in JSON must escape it.
func appendStackTrace(buf []byte, service string, rng *rand.Rand) []byte {
	switch rng.IntN(stackTraceKinds) {
	case 0:
		return appendPyTrace(buf, service, rng)
	case 1:
		return appendJavaTrace(buf, service, rng)
	default:
		return appendGoTrace(buf, service, rng)
	}
}

func appendPyTrace(buf []byte, service string, rng *rand.Rand) []byte {
	exc := pyExceptions[rng.IntN(len(pyExceptions))]
	buf = append(buf, "Traceback (most recent call last):"...)
	frames := 2 + rng.IntN(5)
	for i := 0; i < frames; i++ {
		buf = append(buf, "\n  File \""...)
		if i == 0 {
			buf = append(buf, "/app/services/"...)
			buf = append(buf, service...)
			buf = append(buf, ".py"...)
		} else {
			buf = append(buf, "/usr/lib/python3.12/site-packages/"...)
			buf = append(buf, pyLibs[rng.IntN(len(pyLibs))]...)
			buf = append(buf, ".py"...)
		}
		buf = append(buf, "\", line "...)
		buf = strconv.AppendInt(buf, int64(20+rng.IntN(900)), 10)
		buf = append(buf, ", in "...)
		buf = append(buf, pyFuncs[rng.IntN(len(pyFuncs))]...)
		buf = append(buf, "\n    "...)
		buf = append(buf, pyCode[rng.IntN(len(pyCode))]...)
	}
	buf = append(buf, '\n')
	buf = append(buf, exc.kind...)
	buf = append(buf, ": "...)
	buf = append(buf, exc.msg...)
	return buf
}

func appendJavaTrace(buf []byte, service string, rng *rand.Rand) []byte {
	exc := javaExceptions[rng.IntN(len(javaExceptions))]
	buf = append(buf, exc.kind...)
	buf = append(buf, ": "...)
	buf = append(buf, exc.msg...)
	frames := 3 + rng.IntN(6)
	for i := 0; i < frames; i++ {
		cls := javaClasses[rng.IntN(len(javaClasses))]
		buf = append(buf, "\n\tat com.datadoghq."...)
		buf = append(buf, service...)
		buf = append(buf, '.')
		buf = append(buf, cls...)
		buf = append(buf, '.')
		buf = append(buf, javaMethods[rng.IntN(len(javaMethods))]...)
		buf = append(buf, '(')
		buf = append(buf, cls...)
		buf = append(buf, ".java:"...)
		buf = strconv.AppendInt(buf, int64(10+rng.IntN(400)), 10)
		buf = append(buf, ')')
	}
	buf = append(buf, "\n\tat java.base/java.util.concurrent.ThreadPoolExecutor.runWorker(ThreadPoolExecutor.java:1144)"...)
	buf = append(buf, "\n\tat java.base/java.lang.Thread.run(Thread.java:840)"...)
	if rng.IntN(2) == 0 {
		cause := javaExceptions[rng.IntN(len(javaExceptions))]
		buf = append(buf, "\nCaused by: "...)
		buf = append(buf, cause.kind...)
		buf = append(buf, ": "...)
		buf = append(buf, cause.msg...)
		buf = append(buf, "\n\tat com.datadoghq."...)
		buf = append(buf, service...)
		buf = append(buf, ".internal.Retry.attempt(Retry.java:"...)
		buf = strconv.AppendInt(buf, int64(10+rng.IntN(200)), 10)
		buf = append(buf, ")\n\t... "...)
		buf = strconv.AppendInt(buf, int64(4+rng.IntN(20)), 10)
		buf = append(buf, " more"...)
	}
	return buf
}

func appendGoTrace(buf []byte, service string, rng *rand.Rand) []byte {
	buf = append(buf, "panic: "...)
	buf = append(buf, goPanics[rng.IntN(len(goPanics))]...)
	buf = append(buf, "\n\ngoroutine "...)
	buf = strconv.AppendInt(buf, int64(1+rng.IntN(500)), 10)
	buf = append(buf, " [running]:"...)
	frames := 2 + rng.IntN(5)
	for i := 0; i < frames; i++ {
		buf = append(buf, "\nmain."...)
		buf = append(buf, goFuncs[rng.IntN(len(goFuncs))]...)
		buf = append(buf, "(0xc000"...)
		buf = appendHex(buf, rng.Uint32()&0xffffff, 6)
		buf = append(buf, ")\n\t/app/"...)
		buf = append(buf, service...)
		buf = append(buf, "/server.go:"...)
		buf = strconv.AppendInt(buf, int64(10+rng.IntN(600)), 10)
		buf = append(buf, " +0x"...)
		buf = appendHex(buf, rng.Uint32()&0xfff, 0)
	}
	buf = append(buf, "\nnet/http.HandlerFunc.ServeHTTP(...)\n\t/usr/local/go/src/net/http/server.go:2166 +0x29"...)
	return buf
}

func appendHex(buf []byte, v uint32, width int) []byte {
	var tmp [16]byte
	s := strconv.AppendUint(tmp[:0], uint64(v), 16)
	for i := len(s); i < width; i++ {
		buf = append(buf, '0')
	}
	return append(buf, s...)
}

// exceptionSummary returns the "kind: message" pair used for the JSON error
// object. It is independent of the trace flavour chosen for the stack, so the
// same RNG draw sequence is consumed in both plain and JSON output.
func pickException(rng *rand.Rand) exception {
	return pyExceptions[rng.IntN(len(pyExceptions))]
}

const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// randomPool returns n bytes of random alphanumerics; wide lines and padding
// take random windows of it so they are cheap to produce but not trivially
// compressible.
func randomPool(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = alnum[rng.IntN(len(alnum))]
	}
	return b
}
