package main

import (
	"fmt"
	mrand "math/rand"
	"strings"
)

// LogKind is a content domain the operator can tick in the UI. It only shapes the
// *text* of a record (headline message + padding filler) — the wire format is still
// owned by the LogFormatter. Content must not contain `"` or `\` so it drops into
// JSON / logfmt quoted fields without escaping on the hot path.
type LogKind struct {
	Key      string
	Label    string
	Messages []string // headline messages, e.g. "GET /api/orders 200"
	Filler   []string // key=value detail tokens used to pad to targetBytes
}

var logKinds = []LogKind{
	{
		Key: "http", Label: "HTTP access",
		Messages: []string{
			"GET /api/orders 200", "POST /api/checkout 201", "GET /api/users/42 404",
			"PUT /api/cart 200", "DELETE /api/session 204", "GET /healthz 200",
			"POST /api/login 401", "GET /api/products 500", "GET /api/search 504",
		},
		Filler: []string{
			"method=GET", "method=POST", "status=200", "status=404", "status=502",
			"latency_ms=12", "latency_ms=87", "latency_ms=1450", "bytes_out=5120",
			"client_ip=10.0.4.17", "client_ip=172.16.9.3", "user_agent=Mozilla/5.0",
			"user_agent=curl/8.4.0", "referer=https://shop.example.com/cart",
			"route=/api/orders/:id", "upstream=orders-api:8080", "proto=HTTP/2",
		},
	},
	{
		Key: "database", Label: "Database",
		Messages: []string{
			"query completed", "slow query detected", "connection pool exhausted",
			"deadlock detected, retrying transaction", "replica lag above threshold",
			"migration applied", "connection acquired", "transaction rolled back",
		},
		Filler: []string{
			"db=postgres", "db=mysql", "table=orders", "table=users", "rows=1",
			"rows=482", "duration_ms=3", "duration_ms=2310", "pool_active=48",
			"pool_idle=2", "statement=SELECT", "statement=UPDATE", "statement=INSERT",
			"lag_seconds=14", "isolation=read_committed", "txn_id=88123",
		},
	},
	{
		Key: "auth", Label: "Auth / security",
		Messages: []string{
			"login succeeded", "login failed: invalid password", "MFA challenge issued",
			"token refreshed", "token expired", "account locked after failed attempts",
			"permission denied", "password reset requested", "suspicious login location",
		},
		Filler: []string{
			"user=alice", "user=bob", "user=svc-deploy", "auth_method=password",
			"auth_method=oidc", "auth_method=api_key", "mfa=totp", "attempts=5",
			"src_ip=203.0.113.24", "src_ip=198.51.100.7", "geo=DE", "geo=US",
			"scope=orders:write", "role=admin", "session_ttl=3600", "outcome=deny",
		},
	},
	{
		Key: "payment", Label: "Payments",
		Messages: []string{
			"payment authorized", "payment captured", "payment declined",
			"refund issued", "chargeback received", "3DS verification required",
			"gateway timeout, retrying", "invoice generated",
		},
		Filler: []string{
			"amount=49.99", "amount=1200.00", "currency=USD", "currency=EUR",
			"gateway=stripe", "gateway=adyen", "card_brand=visa", "card_brand=amex",
			"decline_code=insufficient_funds", "order_id=ORD-77812", "merchant=acme",
			"risk_score=12", "risk_score=87", "attempt=2",
		},
	},
	{
		Key: "kubernetes", Label: "Kubernetes / infra",
		Messages: []string{
			"pod started", "container OOMKilled", "readiness probe failed",
			"back-off restarting failed container", "node pressure: disk",
			"scaled deployment", "image pulled", "volume mount failed",
		},
		Filler: []string{
			"namespace=prod", "namespace=staging", "pod=orders-api-7d9f8c-x2kq",
			"pod=cart-worker-5b6c4-9zt1", "node=ip-10-0-3-21", "container=app",
			"restarts=7", "memory_limit=512Mi", "cpu_limit=500m", "replicas=6",
			"image=registry.local/orders:1.14.2", "reason=CrashLoopBackOff",
		},
	},
	{
		Key: "queue", Label: "Queue / messaging",
		Messages: []string{
			"message published", "message consumed", "consumer lag increasing",
			"message moved to dead-letter queue", "rebalance triggered",
			"ack timeout, redelivering", "partition leader changed",
		},
		Filler: []string{
			"broker=kafka", "broker=rabbitmq", "topic=orders.created",
			"topic=payments.events", "partition=3", "offset=1048576", "lag=2300",
			"consumer_group=billing", "delivery_attempt=4", "msg_bytes=812",
		},
	},
	{
		Key: "cache", Label: "Cache",
		Messages: []string{
			"cache hit", "cache miss", "cache eviction", "key expired",
			"cache warmup complete", "redis connection reset",
		},
		Filler: []string{
			"cache=redis", "cache=memcached", "key=user:42:profile",
			"key=cart:9f31", "ttl=300", "hit_ratio=0.93", "evicted=128",
			"mem_used_mb=812", "latency_us=140",
		},
	},
}

// KindMeta is what /meta returns so the UI checkbox list is server-driven.
type KindMeta struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

func KindMetadata() []KindMeta {
	out := make([]KindMeta, len(logKinds))
	for i, k := range logKinds {
		out[i] = KindMeta{Key: k.Key, Label: k.Label}
	}
	return out
}

func lookupKind(key string) *LogKind {
	for i := range logKinds {
		if logKinds[i].Key == key {
			return &logKinds[i]
		}
	}
	return nil
}

// LogContent is the per-job text pool formatters draw from. It is built once at Start
// and is read-only afterwards, so workers share it without locking.
type LogContent struct {
	kinds []*LogKind // empty => generic (legacy lorem-ipsum pools)
}

// genericContent preserves the original behaviour for requests with no kinds.
var genericContent = &LogContent{}

// NewLogContent validates keys and builds the pool. Unknown keys are an error so a
// typo in an API call doesn't silently fall back to lorem ipsum.
func NewLogContent(keys []string) (*LogContent, error) {
	if len(keys) == 0 {
		return genericContent, nil
	}
	c := &LogContent{}
	for _, k := range keys {
		kind := lookupKind(strings.TrimSpace(k))
		if kind == nil {
			return nil, fmt.Errorf("unknown log kind %q", k)
		}
		c.kinds = append(c.kinds, kind)
	}
	return c, nil
}

// pickKind chooses which selected kind the next record (or filler token) comes from.
func (c *LogContent) pickKind() *LogKind {
	return c.kinds[mrand.Intn(len(c.kinds))]
}

func (c *LogContent) message() string {
	if c == nil || len(c.kinds) == 0 {
		return pick(sampleMessages)
	}
	return pick(c.pickKind().Messages)
}

func (c *LogContent) fillerWord() string {
	if c == nil || len(c.kinds) == 0 {
		return pick(fillerWords)
	}
	return pick(c.pickKind().Filler)
}
