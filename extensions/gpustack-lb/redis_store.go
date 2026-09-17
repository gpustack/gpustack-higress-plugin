package main

import (
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/resp"
)

// The redis backend for the shared state.
//
// **What it buys**: shared data is scoped to one Envoy process, and a gateway
// runs one wasm VM per worker thread, so even a single-replica deployment
// already keeps several independent in-flight counts and health verdicts.
// Redis makes both facts deployment-wide: least-load sees the real aggregate,
// maxRunningRequests becomes a fleet-wide soft cap instead of a per-process
// one, and one replica ejecting a dead instance ejects it for all of them.
//
// **What it costs**: the context role has to wait for the read, so every LB
// request carries one blocking round-trip (returns
// HeaderStopAllIterationAndWatermark, resumes in the callback). This is the
// shape Higress's own prefix_cache uses and the one gpustack-rate-limit already
// runs in production; against an LLM request measured in seconds, an in-cluster
// round-trip of well under a millisecond is noise. The two writes are
// fire-and-forget and never block anything.
//
// **Fail-open on every failure.** A dispatch error, a redis error reply, a
// timeout or a malformed reply all resolve to "no state": every candidate is
// published with zero in-flight and a clean health record, so selection
// degrades to the weighted dice roll / round-robin it would have done before
// any of this existed. That is a deliberate choice of the lesser evil -- for
// the duration of a redis outage there is no ejection either, so the black-hole
// effect §6.1 describes can come back. Taking every LB route out of service
// because a cache is down is worse.

const (
	// defaultRedisKeyPrefix also acts as the Redis Cluster **hash tag** -- see
	// redisStore.inflightKey.
	defaultRedisKeyPrefix = "gpustack_lb"

	defaultRedisPort      = 6379
	staticRedisPort       = 80
	maxTCPPort            = 65535
	defaultRedisTimeoutMs = 1000

	// minInflightTTLSeconds floors the sorted-set TTL so that a very small
	// maxInflightAgeMs cannot produce a key that expires between the +1 and the
	// -1 of a single request.
	minInflightTTLSeconds = 60

	// minHealthTTLSeconds floors the health-hash TTL. The hash holds the
	// **consecutive** failure count, so letting it expire is not a correctness
	// problem -- it just forgets blips that were never going to add up to an
	// ejection anyway, which is the desired reading of "consecutive".
	minHealthTTLSeconds = 3600
)

// redisSettings is the parsed `redis` config block.
//
// Field names follow gpustack-rate-limit and the rest of the Higress plugin
// ecosystem (snake_case), not gpustack-lb's own camelCase: an operator
// configuring redis is copying a block they already have, and the reconciler
// that generates one can generate both from the same code.
type redisSettings struct {
	ServiceName string
	ServicePort int
	Username    string
	Password    string
	TimeoutMs   int
	Database    int
	KeyPrefix   string
}

// parseRedisSettings is pure (no host calls) so the whole validation surface is
// unit-testable; newRedisStore does the part that needs a wasm host.
func parseRedisSettings(j gjson.Result) (redisSettings, error) {
	var s redisSettings

	s.ServiceName = j.Get("service_name").String()
	if s.ServiceName == "" {
		return s, errors.New("redis.service_name must not be empty")
	}

	// The upper bound matters as much as the lower one: an out-of-range port
	// produces a cluster name that simply never resolves, and the only symptom
	// is every state load timing out into the fail-open path -- which looks
	// exactly like "redis is down" rather than "this number is not a port".
	s.ServicePort = int(j.Get("service_port").Int())
	if s.ServicePort < 0 || s.ServicePort > maxTCPPort {
		return s, fmt.Errorf("redis.service_port %d must be in [0, %d]", s.ServicePort, maxTCPPort)
	}
	if s.ServicePort == 0 {
		// A `.static` service is a Higress static-DNS registration, whose
		// listener port is 80 regardless of what the backend speaks.
		if strings.HasSuffix(s.ServiceName, ".static") {
			s.ServicePort = staticRedisPort
		} else {
			s.ServicePort = defaultRedisPort
		}
	}

	s.Username = j.Get("username").String()
	s.Password = j.Get("password").String()

	s.TimeoutMs = int(j.Get("timeout").Int())
	if s.TimeoutMs < 0 {
		return s, fmt.Errorf("redis.timeout %d must be >= 0", s.TimeoutMs)
	}
	if s.TimeoutMs == 0 {
		s.TimeoutMs = defaultRedisTimeoutMs
	}

	s.Database = int(j.Get("database").Int())
	if s.Database < 0 {
		return s, fmt.Errorf("redis.database %d must be >= 0", s.Database)
	}

	s.KeyPrefix = j.Get("key_prefix").String()
	if s.KeyPrefix == "" {
		s.KeyPrefix = defaultRedisKeyPrefix
	}
	// The prefix is spliced into a Redis Cluster hash tag, so a brace in it
	// would silently change which substring the slot is computed from -- and
	// two deployments could then land on different slots while believing they
	// are isolated by prefix alone.
	if strings.ContainsAny(s.KeyPrefix, "{}") {
		return s, fmt.Errorf("redis.key_prefix %q must not contain braces", s.KeyPrefix)
	}

	return s, nil
}

type redisStore struct {
	client wrapper.RedisClient
	// keyTag is the prefix wrapped in a Redis Cluster hash tag.
	keyTag string
}

func newRedisStore(s redisSettings) (stateStore, error) {
	client := wrapper.NewRedisClusterClient(wrapper.FQDNCluster{
		FQDN: s.ServiceName,
		Port: int64(s.ServicePort),
	})
	if err := client.Init(s.Username, s.Password, int64(s.TimeoutMs), wrapper.WithDataBase(s.Database)); err != nil {
		return nil, err
	}
	return &redisStore{client: client, keyTag: "{" + s.KeyPrefix + "}"}, nil
}

// inflightKey and healthKey both start with the same **hash tag**.
//
// Under Redis Cluster a multi-key script is rejected (CROSSSLOT) unless every
// key hashes to the same slot, and LoadStates deliberately batches all the
// candidates of a route into one script -- that batching is the entire reason
// the read costs one round-trip instead of 2N. Wrapping the prefix in braces
// pins every key this plugin writes to a single slot, which makes the batch
// legal. The sharding given up is worth nothing here: the key count is bounded
// by the number of model instances, and the values are tiny.
func (s *redisStore) inflightKey(cluster string) string { return s.keyTag + ":inflight:" + cluster }
func (s *redisStore) healthKey(cluster string) string   { return s.keyTag + ":health:" + cluster }

// loadScript is **read-only**: it counts the live in-flight members with ZCOUNT
// rather than pruning the expired ones first.
//
// Pruning here would be a write on the read path -- on every request, for every
// candidate, from the role that is supposed to be a reader. The finisher prunes
// on both of its writes instead, so expired members are cleaned up by the very
// traffic that could create them, and a key nobody touches any more simply
// expires.
//
// Every value comes back as a string so the Go side has one shape to decode
// rather than a mix of RESP integers and bulk strings. The count is formatted
// with **`%d`, not `tostring`**: Lua 5.1's tostring renders a number through
// `%.14g`, which switches to scientific notation ("1e+15") past its precision,
// and the decoder would reject that.
const loadScript = `
local out = {}
local live = '(' .. ARGV[1]
for i = 1, #KEYS, 2 do
  out[#out+1] = string.format('%d', redis.call('ZCOUNT', KEYS[i], live, '+inf'))
  local h = redis.call('HMGET', KEYS[i+1], 'f', 'e', 'r')
  out[#out+1] = h[1] or '0'
  out[#out+1] = h[2] or '0'
  out[#out+1] = h[3] or '0'
end
return out
`

// addScript records one in-flight request.
//
// ARGV: [1] now ms, [2] cutoff ms (anything at or below it has aged out),
// [3] member, [4] key TTL in seconds.
//
// The sorted set replaces the shared-data backend's timestamp array wholesale:
// ZADD/ZREM/ZREMRANGEBYSCORE need no read-modify-write, so there is no CAS loop,
// no retry budget, and none of the "the first write to a new key bypasses CAS"
// window that state.go has to document.
const addScript = `
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[2])
redis.call('ZADD', KEYS[1], ARGV[1], ARGV[3])
redis.call('EXPIRE', KEYS[1], ARGV[4])
return 1
`

// doneScript releases the in-flight entry and records the health outcome in one
// round-trip, because they always happen together in the same hook.
//
// ARGV: [1] cutoff ms, [2] member (empty = nothing to release),
// [3] in-flight TTL, [4] outcome (empty | 'ok' | 'fail'), [5] threshold,
// [6] ejected-until ms, [7] ramp-until ms, [8] health TTL.
//
// ⚠️ **The two deadlines are computed in Go and arrive as strings**; the script
// must not do that arithmetic itself. Redis serialises a Lua **number**
// argument through a shortest-round-trip float formatter, so a millisecond
// timestamp that happens to need few significant digits (1700000000000) is
// written as "1.7e+12" -- which the decoder then rejects, discarding the state
// of every cluster in that reply. Passing them straight through as opaque
// strings removes the number-to-string conversion entirely.
//
// The success branch writes **only when the count is non-zero**, so the
// overwhelming majority of requests touch nothing, and a cluster that has never
// failed never gets a health key at all. The failure branch is a single atomic
// HINCRBY, which is strictly better than the CAS loop it replaces: a lost reset
// there let non-consecutive failures accumulate into an ejection.
const doneScript = `
if ARGV[2] ~= '' then
  redis.call('ZREM', KEYS[1], ARGV[2])
end
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
redis.call('EXPIRE', KEYS[1], ARGV[3])
if ARGV[4] == 'ok' then
  local f = redis.call('HGET', KEYS[2], 'f')
  if f and f ~= '0' then
    redis.call('HSET', KEYS[2], 'f', 0)
    redis.call('EXPIRE', KEYS[2], ARGV[8])
  end
elseif ARGV[4] == 'fail' then
  local f = redis.call('HINCRBY', KEYS[2], 'f', 1)
  if f >= tonumber(ARGV[5]) then
    redis.call('HSET', KEYS[2], 'f', 0, 'e', ARGV[6], 'r', ARGV[7])
  end
  redis.call('EXPIRE', KEYS[2], ARGV[8])
end
return 1
`

const (
	outcomeArgSuccess = "ok"
	outcomeArgFailure = "fail"
	outcomeArgIgnored = ""
)

func outcomeArg(oc outcome) string {
	switch oc {
	case outcomeSuccess:
		return outcomeArgSuccess
	case outcomeFailure:
		return outcomeArgFailure
	default:
		return outcomeArgIgnored
	}
}

func (s *redisStore) LoadStates(clusters []string, nowMs, maxAgeMs int64, done func(map[string]clusterState)) types.Action {
	if len(clusters) == 0 {
		done(nil)
		return types.ActionContinue
	}

	keys := make([]interface{}, 0, len(clusters)*2)
	for _, cluster := range clusters {
		keys = append(keys, s.inflightKey(cluster), s.healthKey(cluster))
	}
	args := []interface{}{strconv.FormatInt(inflightCutoff(nowMs, maxAgeMs), 10)}

	// **The callback owns the resume**, and it is the only path that resumes:
	// the wrapper invokes it exactly once per successful dispatch, including
	// when redis answered with an error or timed out (the error is converted
	// into a resp error value rather than swallowed). That is what makes the
	// pause safe -- there is no branch on which the request stays parked.
	err := s.client.Eval(loadScript, len(keys), keys, args, func(response resp.Value) {
		states, decodeErr := decodeLoadReply(clusters, response)
		if decodeErr != nil {
			proxywasm.LogWarnf("%s: redis state load failed, continuing without state: %v", pluginName, decodeErr)
		}
		done(states)
		if err := proxywasm.ResumeHttpRequest(); err != nil {
			// Only reachable in a host-side anomaly (the stream is no longer
			// paused, the VM is being torn down). The request then hangs until
			// Envoy's idle timeout; a log line is all the signal an operator
			// gets.
			proxywasm.LogWarnf("%s: ResumeHttpRequest failed after state load: %v", pluginName, err)
		}
	})
	if err != nil {
		// The dispatch never happened, so no callback is coming and the request
		// was never paused. Publish an empty state and carry on -- see the
		// fail-open note at the top of this file.
		proxywasm.LogWarnf("%s: redis state load dispatch failed, continuing without state: %v", pluginName, err)
		done(nil)
		return types.ActionContinue
	}
	return types.HeaderStopAllIterationAndWatermark
}

func (s *redisStore) AddInflight(cluster string, nowMs, maxAgeMs int64) string {
	member := newInflightToken(nowMs)
	keys := []interface{}{s.inflightKey(cluster)}
	args := []interface{}{
		strconv.FormatInt(nowMs, 10),
		strconv.FormatInt(inflightCutoff(nowMs, maxAgeMs), 10),
		member,
		strconv.FormatInt(inflightTTLSeconds(maxAgeMs), 10),
	}
	// Fire-and-forget: a nil callback keeps this off the request's critical
	// path. The consequence is that the increment is not guaranteed to be
	// visible to a concurrent request being published right now -- the same
	// window the shared-data backend has between the read at 795 and the write
	// at 700, which is why maxRunningRequests is documented as a soft cap.
	if err := s.client.Eval(addScript, len(keys), keys, args, nil); err != nil {
		proxywasm.LogWarnf("%s: redis inflight add for %s failed: %v", pluginName, cluster, err)
		// Returning an empty token makes Done skip the release. That is the
		// correct pairing: there is nothing to release, and ZREM of a member
		// that was never added would be a wasted round-trip.
		return ""
	}
	return member
}

func (s *redisStore) Done(rec doneRecord) {
	keys := []interface{}{s.inflightKey(rec.Cluster), s.healthKey(rec.Cluster)}
	if err := s.client.Eval(doneScript, len(keys), keys, doneArgs(rec), nil); err != nil {
		proxywasm.LogWarnf("%s: redis done for %s failed: %v", pluginName, rec.Cluster, err)
	}
}

// doneArgs builds doneScript's ARGV. Split out as a pure function because it is
// eight positional slots that have to line up with the script by index, and
// because it is where the millisecond timestamps get formatted -- the one thing
// the script deliberately does not do.
//
// Both deadlines are stamped here, exactly as the shared-data backend stamps
// them, so the reader derives the penalty and probation from the state alone
// and never needs to know cooldownMs / rampMs.
func doneArgs(rec doneRecord) []interface{} {
	ejectedUntil := rec.NowMs + rec.CooldownMs
	return []interface{}{
		strconv.FormatInt(inflightCutoff(rec.NowMs, rec.MaxAgeMs), 10),
		rec.Token,
		strconv.FormatInt(inflightTTLSeconds(rec.MaxAgeMs), 10),
		outcomeArg(rec.Outcome),
		strconv.FormatInt(rec.Threshold, 10),
		strconv.FormatInt(ejectedUntil, 10),
		strconv.FormatInt(ejectedUntil+rec.RampMs, 10),
		strconv.FormatInt(healthTTLSeconds(rec.CooldownMs, rec.RampMs), 10),
	}
}

// inflightCutoff is the score at or below which an in-flight entry has leaked
// and no longer counts. Shared by the reader's ZCOUNT lower bound and the
// writer's ZREMRANGEBYSCORE upper bound, so the two can never disagree about
// which entries are live.
func inflightCutoff(nowMs, maxAgeMs int64) int64 { return nowMs - maxAgeMs }

func inflightTTLSeconds(maxAgeMs int64) int64 {
	// Twice the leak horizon: past that point every member in the set would be
	// expired anyway, so letting the key go costs nothing and keeps clusters
	// that have been removed from the topology from lingering forever.
	ttl := maxAgeMs * 2 / 1000
	if ttl < minInflightTTLSeconds {
		return minInflightTTLSeconds
	}
	return ttl
}

func healthTTLSeconds(cooldownMs, rampMs int64) int64 {
	// The TTL has to outlive an in-progress ejection, otherwise the key
	// evaporates mid-cooldown and the instance silently returns to the
	// candidate set early.
	ttl := (cooldownMs + rampMs) * 2 / 1000
	if ttl < minHealthTTLSeconds {
		return minHealthTTLSeconds
	}
	return ttl
}

// newInflightToken is the sorted-set member identifying one in-flight request.
//
// It must be unique: two requests sharing a member would collapse into one ZSET
// entry, so the cluster would under-report its load and the first of the two to
// finish would release both. The timestamp prefix is only there to make a
// `ZRANGE` dump readable while troubleshooting; the random half is what carries
// the uniqueness.
//
// **The global math/rand is the right generator here, and a per-store
// time-seeded rand.Rand would be a downgrade.** The concern it answers is that
// Envoy runs one wasm VM per worker thread, so an identically-seeded generator
// would have every thread emit the same sequence and two requests landing on
// one cluster in the same millisecond would mint the same member. That was real
// up to Go 1.19, whose global source was seeded with the constant 1. It is not
// how this binary behaves: the module declares go 1.24.4, so the randautoseed=0
// compatibility knob does not apply, and rand.Uint64 resolves to the runtime's
// ChaCha8 generator seeded in randinit() from readRandom -- which on wasip1 is
// the WASI random_get host import. Two consequences worth stating:
//
//   - If the host did not export random_get the module would fail to
//     instantiate, so "the plugin is running" already implies real entropy.
//   - If random_get resolved but errored, the runtime falls back to seeding
//     from the clock -- which is precisely the weaker scheme a hand-rolled
//     rand.New(rand.NewSource(time.Now().UnixNano())) would make the *only*
//     scheme. Under a host whose clock_time_get is coarse, several VMs coming
//     up together would then share a seed outright: the very collision the
//     change is meant to prevent, reintroduced by the fix.
//
// selector.go's rrCursor rests on the same property for the same reason (worker
// threads must not march in step), so the two stand or fall together.
func newInflightToken(nowMs int64) string {
	return strconv.FormatInt(nowMs, 36) + "-" + strconv.FormatUint(rand.Uint64(), 36)
}

// loadReplyFields is how many values loadScript emits per cluster: in-flight
// count, then the three health fields.
const loadReplyFields = 4

// decodeLoadReply turns loadScript's flat reply into per-cluster state.
//
// **Pure** -- no host calls, not even for logging (the caller logs), which is
// what makes the script's wire contract unit-testable without a redis or a wasm
// host. Same rule as normalizeMode in config.go.
//
// **Every failure resolves to a nil map**, not to a partial one: an error
// reply, a short array or a value that will not parse all mean the same thing
// downstream -- publish every candidate with no load and no health verdict.
// Decoding half of it would be worse than decoding none, because the candidates
// that did decode would carry real in-flight counts while the rest carried
// zero, and least-load would then steer everything at whichever half failed.
func decodeLoadReply(clusters []string, response resp.Value) (map[string]clusterState, error) {
	if err := response.Error(); err != nil {
		return nil, err
	}
	arr := response.Array()
	if len(arr) != len(clusters)*loadReplyFields {
		return nil, fmt.Errorf("expected %d values for %d clusters, got %d",
			len(clusters)*loadReplyFields, len(clusters), len(arr))
	}
	states := make(map[string]clusterState, len(clusters))
	for i, cluster := range clusters {
		fields := arr[i*loadReplyFields : (i+1)*loadReplyFields]
		var (
			st  clusterState
			err error
		)
		if st.Inflight, err = strconv.ParseInt(fields[0].String(), 10, 64); err != nil {
			return nil, fmt.Errorf("unparseable inflight for %s: %w", cluster, err)
		}
		if st.Health.Fails, err = strconv.ParseInt(fields[1].String(), 10, 64); err != nil {
			return nil, fmt.Errorf("unparseable fail count for %s: %w", cluster, err)
		}
		if st.Health.EjectedUntil, err = strconv.ParseInt(fields[2].String(), 10, 64); err != nil {
			return nil, fmt.Errorf("unparseable ejection deadline for %s: %w", cluster, err)
		}
		if st.Health.RampUntil, err = strconv.ParseInt(fields[3].String(), 10, 64); err != nil {
			return nil, fmt.Errorf("unparseable ramp deadline for %s: %w", cluster, err)
		}
		states[cluster] = st
	}
	return states, nil
}
