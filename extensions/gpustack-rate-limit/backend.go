package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/resp"
)

// LimitEntry describes one combination's sliding-window operation for a BatchEval call.
type LimitEntry struct {
	Key    string // pre-built rate-limit key
	Window int64  // sliding-window size in seconds
	Quota  int    // max count/tokens allowed within the window
	Mode   string // modeCheckAndAdd | modeCheck | modeAdd
	Member string // unique per-request identifier (uuid)
	Count  int64  // 1 for QueryLimits; actual total_tokens for TokenLimits/TokenQuota
	TTL    int64  // backend key expiry in seconds

	// Metric labels: pre-computed alongside Key in collectChecks so the
	// metrics path doesn't need to re-parse the Redis key string.
	Combo  string // combination name from config (combo.Name)
	Kind   string // metricKindQuery / metricKindTokenRolling / metricKindTokenCalendar
	Period string // window/period suffix used in the Redis key, e.g. "1s", "60s", "each_month"
	Bucket string // dimension fragment (everything past <rule>|<combo>) joined with '|'; sanitized at emit time
}

// EvalResult is the outcome of a BatchEval call.
type EvalResult struct {
	Status     string // luaSuccessReply ("SUCCESS") or luaLimitedReply ("LIMITED")
	LimitedKey string // only populated when Status == luaLimitedReply
}

// LimitBackend abstracts the rate-limit storage backend.
//
// BatchEval executes a batch of sliding-window rate-limit checks and/or adds
// and delivers the result via callback. The returned types.Action must be
// returned from the calling HTTP-filter handler:
//   - redisBackend: HeaderStopAllIterationAndWatermark (async; callback fired later)
//   - localBackend: ActionContinue (sync; callback already fired before return)
//
// Semantics mirror multi_level_limit.lua:
//
//	Pass 1 – for every check/check_and_add entry: prune stale members, sum counts,
//	         compare to quota. On the first over-quota entry, invoke callback with
//	         LIMITED and return; no entries are written.
//	Pass 2 – for every check_and_add/add entry: write count with TTL.
//
// Passing a nil callback is valid (fire-and-forget; used in the response phase).
type LimitBackend interface {
	BatchEval(now int64, entries []LimitEntry, callback func(EvalResult)) (types.Action, error)
}

// luaLimitedReply is the status string returned by the Lua script when a quota is exceeded.
const luaLimitedReply = "LIMITED"

// ============================================================
// Redis backend
// ============================================================

type redisBackend struct {
	client wrapper.RedisClient
}

func (b *redisBackend) BatchEval(now int64, entries []LimitEntry, callback func(EvalResult)) (types.Action, error) {
	keys := make([]interface{}, 0, len(entries))
	args := []interface{}{strconv.FormatInt(now, 10)}
	for _, e := range entries {
		keys = append(keys, e.Key)
		args = append(args,
			strconv.FormatInt(e.Window, 10),
			strconv.Itoa(e.Quota),
			e.Mode,
			e.Member,
			strconv.FormatInt(e.Count, 10),
			strconv.FormatInt(e.TTL, 10),
		)
	}
	var cb func(resp.Value)
	if callback != nil {
		cb = func(response resp.Value) {
			result := response.Array()
			var er EvalResult
			if len(result) < 1 {
				// A malformed lua reply means lua errored or the SDK couldn't
				// parse it. Fail-open: treat as success and resume.
				proxywasm.LogWarnf("%s: malformed lua reply (len=%d), fail-open", pluginName, len(result))
				er = EvalResult{Status: luaSuccessReply}
			} else {
				limitedKey := ""
				if len(result) >= 2 {
					limitedKey = result[1].String()
				}
				er = EvalResult{Status: result[0].String(), LimitedKey: limitedKey}
			}
			callback(er)
			// The async path returned HeaderStopAllIterationAndWatermark up
			// front, so the request is paused waiting for us. On success we
			// must explicitly resume it; on rejection the callback's
			// SendHttpResponseWithDetail (inside callback->rejected) already
			// terminates the request and Resume must NOT fire (it would
			// race with the local-reply queueing on some Higress builds).
			if er.Status == luaSuccessReply {
				if err := proxywasm.ResumeHttpRequest(); err != nil {
					// Resume can only fail in a host-side anomaly (the
					// stream is no longer in a Pause state, the wasm VM
					// is being torn down, etc.). The request is then stuck
					// until Envoy's idle timeout fires; logging is the
					// only signal an operator gets.
					proxywasm.LogWarnf("%s: ResumeHttpRequest failed after lua SUCCESS: %v", pluginName, err)
				}
			}
		}
	}
	if err := b.client.Eval(multiLevelLimitScript, len(keys), keys, args, cb); err != nil {
		return types.ActionContinue, err
	}
	return types.HeaderStopAllIterationAndWatermark, nil
}

// ============================================================
// Local backend (proxy-wasm shared data)
// ============================================================

// localMaxCASRetries caps the per-key CAS retry loop. After this many conflicts
// the write is abandoned and a warning is logged (fail-open posture preserved).
const localMaxCASRetries = 10

type localBackend struct{}

// BatchEval implements two-pass sliding-window semantics using proxy-wasm shared data
// with CAS-based optimistic locking.
//
// Pass 1 (check): reads the current window state for every check/check_and_add entry.
// On the first over-quota entry callback(LIMITED) is called and the method returns.
// Pass 2 (write): appends a new entry for every check_and_add/add entry using a
// CAS retry loop for per-key thread safety.
//
// Action semantics: unlike the async redisBackend, the callback fires *synchronously*
// inside this method. When it triggers a rejection it has already invoked
// SendHttpResponseWithDetail, and proxy-wasm requires the filter handler to then
// return ActionPause (== HeaderStopIteration, value 1) so Envoy stops iterating
// and flushes the queued local reply. This mirrors Higress key_rate_limit (C++)
// which returns FilterHeadersStatus::StopIteration after sendLocalResponse. We
// must NOT return HeaderStopAllIterationAndWatermark (4) here -- that is the
// async-wait shape used by redisBackend (apply backpressure, await callback);
// using it after a sync local-reply leaves the body queued but unflushed
// (symptom: response_code=200/429 + bytes_sent=0 + response_code_details
// =via_wasm::...gpustack-rate-limit.rejected).
//
// Cross-key atomicity is best-effort: concurrent workers may briefly over-admit at
// quota boundaries. This is an accepted trade-off for single-instance local mode.
func (b *localBackend) BatchEval(now int64, entries []LimitEntry, callback func(EvalResult)) (types.Action, error) {
	// Pass 1: check
	for i := range entries {
		e := &entries[i]
		if e.Mode == modeAdd {
			continue
		}
		total, err := localWindowTotal(e.Key, now, e.Window)
		if err != nil {
			proxywasm.LogWarnf("%s: local check for key %s failed, fail-open: %v", pluginName, e.Key, err)
			continue
		}
		proxywasm.LogDebugf("%s: local check req=%s key=%s total=%d quota=%d window=%ds",
			pluginName, e.Member, e.Key, total, e.Quota, e.Window)
		if total >= int64(e.Quota) {
			proxywasm.LogDebugf("%s: local limited req=%s key=%s total=%d quota=%d",
				pluginName, e.Member, e.Key, total, e.Quota)
			if callback != nil {
				callback(EvalResult{Status: luaLimitedReply, LimitedKey: e.Key})
			}
			return types.ActionPause, nil
		}
	}
	// Pass 2: write
	for i := range entries {
		e := &entries[i]
		if e.Mode == modeCheck {
			continue
		}
		if err := localAddEntry(e.Key, now, e.Count, e.Window); err != nil {
			proxywasm.LogWarnf("%s: local add for key %s failed: %v", pluginName, e.Key, err)
		}
	}
	// Success path: fire the callback so the plugin can emit request_total
	// {result=passed} and value_total{kind=query} (these live in the callback
	// because they share the same emission code path with the async redis
	// backend). Unlike the redis backend's cb wrapper we do NOT call
	// ResumeHttpRequest here -- the request was never paused (we returned
	// ActionContinue without ever returning a Pause from this backend's
	// success path) and Resume on an unpaused stream is a host-side no-op
	// at best, a wasted hostcall at worst.
	if callback != nil {
		callback(EvalResult{Status: luaSuccessReply})
	}
	return types.ActionContinue, nil
}

// ============================================================
// Shared-data sliding window helpers
// ============================================================

// windowEntry is a single timestamped count record stored in the sliding window.
type windowEntry struct {
	timestamp int64
	count     int64
}

// localWindowTotal reads shared data for key and sums the counts of all entries
// with timestamp in (now-window, now]. Returns 0 when the key does not exist yet.
//
// We deliberately iterate the raw byte buffer instead of going through
// decodeEntries: this is the request-phase hot path (called on every request
// matching the local backend) and decodeEntries would allocate a []windowEntry
// on each call. The wasm VM has a 200 MiB rebuild ceiling, so reducing
// per-request allocation extends VM lifetime. localAddEntry still uses
// decodeEntries because it needs to mutate / filter the slice anyway.
//
// Trailing bytes not aligned to 16 are silently discarded, mirroring
// decodeEntries semantics.
func localWindowTotal(key string, now, window int64) (int64, error) {
	data, _, err := proxywasm.GetSharedData(key)
	if errors.Is(err, types.ErrorStatusNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := now - window
	var total int64
	for i := 0; i+16 <= len(data); i += 16 {
		timestamp := int64(binary.BigEndian.Uint64(data[i:]))
		if timestamp > cutoff {
			total += int64(binary.BigEndian.Uint64(data[i+8:]))
		}
	}
	return total, nil
}

// localAddEntry appends count at timestamp now for key, pruning expired entries
// in the same write. A CAS retry loop provides per-key atomicity across workers.
func localAddEntry(key string, now, count, window int64) error {
	cutoff := now - window
	for attempt := 0; attempt < localMaxCASRetries; attempt++ {
		data, cas, err := proxywasm.GetSharedData(key)
		if err != nil && !errors.Is(err, types.ErrorStatusNotFound) {
			return err
		}
		src := decodeEntries(data)
		// Prune expired entries and append the new one in a single pass.
		dst := src[:0]
		for _, e := range src {
			if e.timestamp > cutoff {
				dst = append(dst, e)
			}
		}
		dst = append(dst, windowEntry{timestamp: now, count: count})
		err = proxywasm.SetSharedData(key, encodeEntries(dst), cas)
		if err == nil {
			return nil
		}
		if errors.Is(err, types.ErrorStatusCasMismatch) {
			proxywasm.LogDebugf("%s: local CAS conflict key=%s attempt=%d, retrying", pluginName, key, attempt+1)
			continue // another worker modified the key; retry with fresh CAS
		}
		return err
	}
	return fmt.Errorf("CAS retry limit exceeded for key %s", key)
}

// encodeEntries serializes []windowEntry as a flat byte slice.
// Each entry occupies 16 bytes: 8-byte big-endian int64 timestamp + 8-byte big-endian int64 count.
func encodeEntries(entries []windowEntry) []byte {
	buf := make([]byte, len(entries)*16)
	for i, e := range entries {
		binary.BigEndian.PutUint64(buf[i*16:], uint64(e.timestamp))
		binary.BigEndian.PutUint64(buf[i*16+8:], uint64(e.count))
	}
	return buf
}

// decodeEntries deserializes a byte slice produced by encodeEntries.
// Trailing bytes not aligned to 16 are silently discarded.
func decodeEntries(data []byte) []windowEntry {
	n := len(data) / 16
	entries := make([]windowEntry, n)
	for i := range entries {
		entries[i].timestamp = int64(binary.BigEndian.Uint64(data[i*16:]))
		entries[i].count = int64(binary.BigEndian.Uint64(data[i*16+8:]))
	}
	return entries
}
