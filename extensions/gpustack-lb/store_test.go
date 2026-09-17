package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/tidwall/gjson"
	"github.com/tidwall/resp"
)

// stubStore is a stateStore that is not sharedDataStore, which is all the
// inheritance tests need: a pointer they can compare identity against.
type stubStore struct{}

func (*stubStore) LoadStates([]string, int64, int64, func(map[string]clusterState)) types.Action {
	return types.ActionContinue
}
func (*stubStore) AddInflight(string, int64, int64) string { return "" }
func (*stubStore) Done(doneRecord)                         {}

// ---------------------------------------------------------------------------
// Backend selection and inheritance
// ---------------------------------------------------------------------------

func TestNoRedisBlockMeansSharedData(t *testing.T) {
	c := parse(t, `{}`)
	if _, ok := c.store.(sharedDataStore); !ok {
		t.Fatalf("store = %T, want sharedDataStore", c.store)
	}
}

// A `redis: null` literal must read as **absent**, not as an empty settings
// object. gjson's Exists() is true for null, and routing that into the parser
// would fail the whole config on "service_name must not be empty" -- which
// rule_matcher swallows, leaving every matchRule to inherit a zero-valued
// global and silently no-op every request.
func TestRedisNullIsAbsentNotEmpty(t *testing.T) {
	for _, raw := range []string{`{}`, `{"redis": null}`} {
		if redisConfigured(gjson.Parse(raw)) {
			t.Errorf("redisConfigured(%s) = true, want false", raw)
		}
		c := parse(t, raw)
		if _, ok := c.store.(sharedDataStore); !ok {
			t.Errorf("%s: store = %T, want sharedDataStore", raw, c.store)
		}
	}
	if !redisConfigured(gjson.Parse(`{"redis": {"service_name": "r.static"}}`)) {
		t.Error("redisConfigured with an object = false, want true")
	}
}

// An empty object is a **misconfiguration**, not "no redis": the operator asked
// for redis and left out the one required field.
func TestEmptyRedisBlockIsAnError(t *testing.T) {
	var c Config
	if err := parseConfig(gjson.Parse(`{"redis": {}}`), &c); err == nil {
		t.Fatal("parseConfig with an empty redis block: want error, got nil")
	}
}

// A value that is neither an object nor null must **fail**, not read as
// absent. Falling back to shared data there would leave the operator believing
// fleet-wide state is on while every replica quietly keeps its own counts, with
// nothing anywhere saying so.
func TestMalformedRedisBlockIsAnError(t *testing.T) {
	for _, raw := range []string{
		`{"redis": []}`,
		`{"redis": "yes"}`,
		`{"redis": 1}`,
		`{"redis": true}`,
	} {
		var c Config
		if err := parseConfig(gjson.Parse(raw), &c); err == nil {
			t.Errorf("parseConfig(%s): want error, got nil (store = %T)", raw, c.store)
		}
	}
}

func TestOverrideInheritsStore(t *testing.T) {
	global := parse(t, `{}`)
	shared := &stubStore{}
	global.store = shared

	var rule Config
	if err := parseOverrideConfig(gjson.Parse(`{"candidates":[{"cluster":"c"}]}`), global, &rule); err != nil {
		t.Fatalf("parseOverrideConfig: %v", err)
	}
	if rule.store != shared {
		t.Fatalf("store = %#v, want the inherited one", rule.store)
	}
}

// A zero-valued global is what rule_matcher leaves behind when the global
// config failed to parse. The rule must keep the shared-data backend it parsed
// for itself rather than ending up with a nil store.
func TestOverrideWithZeroGlobalKeepsSharedData(t *testing.T) {
	var rule Config
	if err := parseOverrideConfig(gjson.Parse(`{"candidates":[{"cluster":"c"}]}`), Config{}, &rule); err != nil {
		t.Fatalf("parseOverrideConfig: %v", err)
	}
	if _, ok := rule.store.(sharedDataStore); !ok {
		t.Fatalf("store = %T, want sharedDataStore", rule.store)
	}
}

// ---------------------------------------------------------------------------
// redis settings
// ---------------------------------------------------------------------------

func TestParseRedisSettingsDefaults(t *testing.T) {
	s, err := parseRedisSettings(gjson.Parse(`{"service_name": "redis.dns"}`))
	if err != nil {
		t.Fatalf("parseRedisSettings: %v", err)
	}
	if s.ServicePort != defaultRedisPort {
		t.Errorf("ServicePort = %d, want %d", s.ServicePort, defaultRedisPort)
	}
	if s.TimeoutMs != defaultRedisTimeoutMs {
		t.Errorf("TimeoutMs = %d, want %d", s.TimeoutMs, defaultRedisTimeoutMs)
	}
	if s.KeyPrefix != defaultRedisKeyPrefix {
		t.Errorf("KeyPrefix = %q, want %q", s.KeyPrefix, defaultRedisKeyPrefix)
	}
}

// A `.static` service is a Higress static-DNS registration whose listener port
// is 80 whatever the backend speaks; defaulting it to 6379 would produce a
// cluster that never connects.
func TestStaticServiceDefaultsToPort80(t *testing.T) {
	s, err := parseRedisSettings(gjson.Parse(`{"service_name": "redis.static"}`))
	if err != nil {
		t.Fatalf("parseRedisSettings: %v", err)
	}
	if s.ServicePort != staticRedisPort {
		t.Errorf("ServicePort = %d, want %d", s.ServicePort, staticRedisPort)
	}
}

func TestParseRedisSettingsRejects(t *testing.T) {
	for name, raw := range map[string]string{
		"no service_name": `{}`,
		"negative port":   `{"service_name":"r","service_port":-1}`,
		// Out of range is as wrong as negative, and worse to diagnose: the
		// cluster name simply never resolves, so every load times out into the
		// fail-open path and it looks like redis is down.
		"port above 65535":  `{"service_name":"r","service_port":65536}`,
		"negative timeout":  `{"service_name":"r","timeout":-1}`,
		"negative database": `{"service_name":"r","database":-1}`,
		// The prefix becomes a Redis Cluster hash tag, so a brace in it would
		// silently move the slot the keys hash to.
		"braced prefix": `{"service_name":"r","key_prefix":"a{b}"}`,
	} {
		if _, err := parseRedisSettings(gjson.Parse(raw)); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

// ---------------------------------------------------------------------------
// redis key layout
// ---------------------------------------------------------------------------

// Every key this plugin writes has to hash to one Redis Cluster slot, because
// LoadStates batches all of a route's candidates into a single multi-key
// script and a cross-slot script is rejected outright.
func TestKeysShareOneHashTag(t *testing.T) {
	s := &redisStore{keyTag: "{gpustack_lb}"}
	keys := []string{
		s.inflightKey("outbound|80||model-1-1.static"),
		s.healthKey("outbound|80||model-1-1.static"),
		s.inflightKey("outbound|80||model-2-9.static"),
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, "{gpustack_lb}:") {
			t.Errorf("key %q does not start with the hash tag", k)
		}
	}
	if keys[0] == keys[1] {
		t.Error("the in-flight and health keys of one cluster collide")
	}
}

// ---------------------------------------------------------------------------
// TTLs and the leak cutoff
// ---------------------------------------------------------------------------

// The reader's lower bound and the writer's pruning bound are the same number,
// so the two can never disagree about which entries are live.
func TestInflightCutoff(t *testing.T) {
	if got := inflightCutoff(1_000_000, 600_000); got != 400_000 {
		t.Errorf("inflightCutoff = %d, want 400000", got)
	}
}

func TestTTLFloors(t *testing.T) {
	// A tiny maxInflightAgeMs must not produce a key that expires between the
	// +1 and the -1 of a single request.
	if got := inflightTTLSeconds(1000); got != minInflightTTLSeconds {
		t.Errorf("inflightTTLSeconds(1000) = %d, want the floor %d", got, minInflightTTLSeconds)
	}
	if got := inflightTTLSeconds(600_000); got != 1200 {
		t.Errorf("inflightTTLSeconds(600000) = %d, want 1200", got)
	}
	// The health TTL has to outlive an in-progress ejection, or the instance
	// silently returns to the candidate set mid-cooldown.
	if got := healthTTLSeconds(10_000, 10_000); got != minHealthTTLSeconds {
		t.Errorf("healthTTLSeconds = %d, want the floor %d", got, minHealthTTLSeconds)
	}
	if got := healthTTLSeconds(3_600_000, 3_600_000); got != 14400 {
		t.Errorf("healthTTLSeconds = %d, want 14400", got)
	}
}

// Two requests sharing a sorted-set member would collapse into one entry: the
// cluster would under-report its load, and whichever finished first would
// release both.
func TestInflightTokensAreUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		tok := newInflightToken(1_700_000_000_000)
		if _, dup := seen[tok]; dup {
			t.Fatalf("duplicate token %q", tok)
		}
		seen[tok] = struct{}{}
	}
}

func TestOutcomeArg(t *testing.T) {
	for oc, want := range map[outcome]string{
		outcomeSuccess: outcomeArgSuccess,
		outcomeFailure: outcomeArgFailure,
		outcomeIgnored: outcomeArgIgnored,
	} {
		if got := outcomeArg(oc); got != want {
			t.Errorf("outcomeArg(%d) = %q, want %q", oc, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// doneScript's ARGV
// ---------------------------------------------------------------------------

// The two deadlines are stamped Go-side and handed to the script as opaque
// strings. They must never come out in scientific notation: Lua would have
// produced "1.7e+12" for a timestamp needing few significant digits, and the
// reader rejects that, discarding the state of every cluster in the reply.
func TestDoneArgsFormatsDeadlinesAsPlainIntegers(t *testing.T) {
	args := doneArgs(doneRecord{
		Cluster:    "a",
		Token:      "tok",
		Outcome:    outcomeFailure,
		NowMs:      1_700_000_000_000, // exactly the shape Lua would have mangled
		MaxAgeMs:   600_000,
		Threshold:  3,
		CooldownMs: 10_000,
		RampMs:     10_000,
	})
	if len(args) != 8 {
		t.Fatalf("doneArgs produced %d values, want the script's 8", len(args))
	}
	// ARGV[6] / ARGV[7]: ejected-until, ramp-until.
	if got := args[5]; got != "1700000010000" {
		t.Errorf("ejected-until = %q, want 1700000010000", got)
	}
	if got := args[6]; got != "1700000020000" {
		t.Errorf("ramp-until = %q, want 1700000020000", got)
	}
	for i, a := range args {
		s, ok := a.(string)
		if !ok {
			t.Fatalf("ARGV[%d] is %T, want a string", i+1, a)
		}
		if strings.ContainsAny(s, "eE.") {
			t.Errorf("ARGV[%d] = %q looks like a float", i+1, s)
		}
	}
	// The cutoff the writer prunes with is the same number the reader counts
	// from, so the two can never disagree about which entries are live.
	if got := args[0]; got != "1699999400000" {
		t.Errorf("cutoff = %q, want 1699999400000", got)
	}
}

// An empty token means the add never wrote anything; the script skips the ZREM
// rather than spending a command on a member that was never added.
func TestDoneArgsPassesEmptyTokenThrough(t *testing.T) {
	args := doneArgs(doneRecord{Cluster: "a", Outcome: outcomeIgnored, MaxAgeMs: 600_000})
	if args[1] != "" {
		t.Errorf("member = %q, want empty", args[1])
	}
	if args[3] != outcomeArgIgnored {
		t.Errorf("outcome = %q, want the ignored sentinel", args[3])
	}
}

// ---------------------------------------------------------------------------
// decodeLoadReply -- the script's wire contract
// ---------------------------------------------------------------------------

func loadReply(values ...string) resp.Value {
	arr := make([]resp.Value, 0, len(values))
	for _, v := range values {
		arr = append(arr, resp.StringValue(v))
	}
	return resp.ArrayValue(arr)
}

func TestDecodeLoadReply(t *testing.T) {
	clusters := []string{"a", "b"}
	states, err := decodeLoadReply(clusters, loadReply(
		"3", "0", "0", "0",
		"1", "2", "1700000000000", "1700000010000",
	))
	if err != nil {
		t.Fatalf("decodeLoadReply: %v", err)
	}
	if states["a"].Inflight != 3 {
		t.Errorf("a.Inflight = %d, want 3", states["a"].Inflight)
	}
	if got := states["b"].Health; got.Fails != 2 || got.EjectedUntil != 1700000000000 || got.RampUntil != 1700000010000 {
		t.Errorf("b.Health = %+v", got)
	}
}

// Every failure resolves to no state at all rather than to a partial map: the
// candidates that did decode would carry real in-flight counts while the rest
// carried zero, and least-load would steer everything at whichever half failed.
func TestDecodeLoadReplyFailsWhole(t *testing.T) {
	clusters := []string{"a", "b"}
	cases := map[string]resp.Value{
		"redis error":   resp.ErrorValue(errors.New("NOSCRIPT")),
		"short array":   loadReply("3", "0", "0", "0"),
		"not an array":  resp.StringValue("OK"),
		"unparseable":   loadReply("3", "0", "0", "0", "x", "0", "0", "0"),
		"empty in-flig": loadReply("3", "0", "0", "0", "", "0", "0", "0"),
	}
	for name, reply := range cases {
		states, err := decodeLoadReply(clusters, reply)
		if err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
		if states != nil {
			t.Errorf("%s: states = %v, want nil", name, states)
		}
	}
}

// ---------------------------------------------------------------------------
// candidateClusters
// ---------------------------------------------------------------------------

// gpustack's several-model-names-on-one-cluster case puts the same cluster in
// the candidate list more than once. Un-deduplicated, the redis backend would
// put the same key in one KEYS array twice.
func TestCandidateClustersDeduplicatesInOrder(t *testing.T) {
	got := candidateClusters([]candidateSpec{
		{Candidate: Candidate{Cluster: "b", TargetID: "1"}},
		{Candidate: Candidate{Cluster: "a", TargetID: "1"}},
		{Candidate: Candidate{Cluster: "b", TargetID: "2"}},
	})
	if len(got) != 2 || got[0] != "b" || got[1] != "a" {
		t.Errorf("candidateClusters = %v, want [b a]", got)
	}
}
