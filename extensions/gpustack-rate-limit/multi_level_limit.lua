-- gpustack-rate-limit multi-dimensional, multi-mode sliding-window script
--
-- KEYS[1..N]: one Redis ZSET key per combination
-- ARGV[1]:    current timestamp (epoch seconds, shared across all combinations)
-- ARGV[2..]:  per-combination 6-tuple [window_seconds, quota, mode, member_id, count, ttl]
--
--   mode:      "check_and_add" check + record     (typical for request-count limits)
--              "check"         check only         (request-phase token check)
--              "add"           record only        (response-phase token recording)
--   member_id: base id for ZADD (usually a per-request UUID)
--   count:     weight to record for this request; 1 for request-count, total_tokens
--              for token-count.
--   ttl:       Redis key TTL in seconds, set on every write (add / check_and_add).
--              Decoupled from "window" because calendar-aligned quotas pass a
--              dynamic short window (now - period_start) which would otherwise
--              produce a near-zero TTL at period start and cause the key to be
--              evicted before the next request arrives. The Go caller computes:
--                rolling : ttl = window * 2
--                calendar: ttl = period_end - now + grace
--
-- The ZSET member is encoded as "<member_id>|<count>" so that the check phase
-- can sum <count> across the window to obtain the real total.
--
-- Return value:
--   "SUCCESS"  every combination passed (and was recorded where applicable)
--   <key>      first combination that is over quota; in this case, no
--              combination is recorded

local now = tonumber(ARGV[1])

-- Parse ARGV into a list of combinations.
local combinations = {}
local i, step = 2, 6
while i <= #ARGV do
    combinations[#combinations + 1] = {
        key    = KEYS[#combinations + 1],
        window = tonumber(ARGV[i]),
        quota  = tonumber(ARGV[i + 1]),
        mode   = ARGV[i + 2],
        member = ARGV[i + 3],
        count  = tonumber(ARGV[i + 4]),
        ttl    = tonumber(ARGV[i + 5]),
    }
    i = i + step
end

-- Sum <count> for every ZSET member inside [from, to].
-- Members are expected in the "<uuid>|<count>" form; legacy members without
-- a trailing "|count" are counted as 1 for forward compatibility.
local function windowTotal(key, from, to)
    local members = redis.call('ZRANGEBYSCORE', key, from, to)
    local total = 0
    for _, m in ipairs(members) do
        local c = string.match(m, '|(%d+)$')
        total = total + (tonumber(c) or 1)
    end
    return total
end

-- Pass 1: purge expired members and check every combination that needs checking.
for _, combo in ipairs(combinations) do
    redis.call('ZREMRANGEBYSCORE', combo.key, '-inf', now - combo.window)
    if combo.mode == 'check' or combo.mode == 'check_and_add' then
        if windowTotal(combo.key, now - combo.window, now) >= combo.quota then
            -- Return as array {status, limited_key} to mirror the shape that
            -- higress cluster-key-rate-limit's Lua script returns (array of
            -- values). The response.Array() callback path is the battle-tested
            -- code path under this Envoy/wasm-go build.
            return {'LIMITED', combo.key}
        end
    end
end

-- Pass 2: record every combination that needs recording (check-only is skipped).
for _, combo in ipairs(combinations) do
    if combo.mode == 'add' or combo.mode == 'check_and_add' then
        redis.call('ZADD', combo.key, now, combo.member .. '|' .. combo.count)
        redis.call('EXPIRE', combo.key, combo.ttl)
    end
end

return {'SUCCESS', ''}
