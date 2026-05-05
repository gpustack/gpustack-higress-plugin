package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
)

// ============================================
// Top-level plugin configuration
// ============================================

// PluginConfig is the plugin-wide configuration.
type PluginConfig struct {
	// RuleName is the rate-limit rule name; used as the Redis key prefix.
	RuleName string `json:"rule_name"`

	// LimitCombinations declares one or more rate-limit combinations.
	LimitCombinations []LimitCombination `json:"limit_combinations"`

	// RejectedCode is the HTTP status code returned when a request is rejected. Defaults to 429.
	RejectedCode int `json:"rejected_code,omitempty"`

	// RejectedMsg is the response body returned when a request is rejected.
	RejectedMsg string `json:"rejected_msg,omitempty"`

	// ShowLimitQuotaHeader toggles whether the limited Redis key is echoed back in
	// a response header on rejection.
	ShowLimitQuotaHeader bool `json:"show_limit_quota_header,omitempty"`

	// EnableOnPathSuffix restricts the plugin to requests whose ":path" ends
	// with one of these suffixes (after stripping the query string). Use "*"
	// to match any path. When neither EnableOnPathSuffix nor EnableOnPathPrefix
	// is present in the config source, a default AI-oriented list is applied
	// (see defaultEnableOnPathSuffix).
	EnableOnPathSuffix []string `json:"enable_on_path_suffix,omitempty"`

	// EnableOnPathPrefix restricts the plugin to requests whose ":path" starts
	// with one of these prefixes. Defaults are applied when not present in the
	// config source (see defaultEnableOnPathPrefix).
	EnableOnPathPrefix []string `json:"enable_on_path_prefix,omitempty"`

	// Timezone is the IANA timezone name used to anchor calendar-aligned
	// quotas (every QuotaSpec under this config). Defaults to "UTC" when
	// empty. Must be loadable via time.LoadLocation. A single deployment
	// almost always has one timezone -- declaring it once at the top level
	// avoids per-spec repetition and prevents accidentally mismatched
	// boundaries across combinations.
	Timezone string `json:"timezone,omitempty"`

	// Backend is the rate-limit storage backend (not part of JSON).
	// Set to a redisBackend when "redis" is present in config, otherwise localBackend.
	Backend LimitBackend `json:"-"`

	// location is the resolved Timezone (or UTC), populated by Validate.
	location *time.Location
}

// ============================================
// Source of the matched attribute (string enum)
// ============================================

// Source enumerates where a dimension value is extracted from.
type Source string

const (
	SourceParam    Source = "param"
	SourceHeader   Source = "header"
	SourceCookie   Source = "cookie"
	SourceIP       Source = "ip"
	SourceConsumer Source = "consumer"
)

// ConsumerHeader is the fixed header that the "consumer" source reads from
// (Higress convention).
const ConsumerHeader = "x-mse-consumer"

// IsValid reports whether s is a recognized Source.
func (s Source) IsValid() bool {
	switch s {
	case SourceParam, SourceHeader, SourceCookie, SourceIP, SourceConsumer:
		return true
	}
	return false
}

// ============================================
// Match value (string alias; the actual match strategy is inferred by
// MatchRule.Compile from the literal form of Value and Source).
//
// Strategy inference precedence:
//  1. value == "*"                      -> wildcard; any input matches
//  2. source == ip                      -> parsed as IP or CIDR; a single IP is treated as /32 or /128
//  3. starts with "regexp_capture:"     -> RE2 regex with at least one capturing group;
//                                          on hit, the FIRST capturing group's content is
//                                          used as the Redis key fragment, so two requests
//                                          whose extracted values differ only in a
//                                          non-captured portion share the same bucket.
//  4. starts with "regexp:"             -> RE2 regex; on hit, the WHOLE extracted value is
//                                          used as the key fragment (so different inputs
//                                          end up in different buckets even if both match).
//  5. otherwise                         -> exact match (case sensitive)
// ============================================

// MatchValue is the raw match value string from configuration.
type MatchValue string

// Literal-value contract used internally by Compile (not exported).
const (
	// regexpPrefix triggers regexp matching (whole extracted value used as key).
	regexpPrefix = "regexp:"
	// regexpCapturePrefix triggers regexp matching with capture-group key extraction.
	regexpCapturePrefix = "regexp_capture:"
	// wildcard triggers wildcard matching.
	wildcard MatchValue = "*"
)

// ============================================
// Multi-dimensional combination configuration
// ============================================

// matchKind is an internal enum that records the resolved match strategy
// for a MatchRule, populated by Compile.
type matchKind int

const (
	kindUncompiled matchKind = iota
	kindExact
	kindWildcard
	kindRegexp
	kindRegexpCapture
	kindIPOrCIDR
)

// MatchRule describes a single dimension match: where to read the value and
// what value to match against. Compile must be called once before Match.
type MatchRule struct {
	// Source is the attribute source.
	Source Source `json:"source"`

	// Name is the source-specific identifier. Semantics vary per source:
	//   - header      HTTP header name (case insensitive)
	//   - param       URL query parameter name
	//   - cookie      Cookie attribute name
	//   - ip          Treated as a header-source special case: the header name
	//                 that carries the real client IP (the upstream filter must
	//                 have injected it, e.g. x-real-ip). When Value is an IP or
	//                 CIDR, Compile routes to the CIDR match path.
	//   - consumer    Ignored; always read from the "x-mse-consumer" header.
	Name string `json:"name,omitempty"`

	// Value is the match value. Its strategy is inferred from Value + Source
	// by Compile (see the file-level comment).
	Value MatchValue `json:"value"`

	// Compile artefacts (unexported; not part of JSON).
	kind           matchKind
	compiledRegexp *regexp.Regexp
	compiledCIDR   *net.IPNet
}

// Compile resolves the match strategy from Value + Source and pre-compiles
// regexp / CIDR data. Must be called once per rule during config loading.
// A non-nil error means the configuration is invalid and the plugin should
// refuse to start.
func (r *MatchRule) Compile() error {
	if !r.Source.IsValid() {
		return fmt.Errorf("invalid source %q", r.Source)
	}
	s := string(r.Value)
	switch {
	case s == string(wildcard):
		r.kind = kindWildcard
		return nil
	case r.Source == SourceIP:
		r.kind = kindIPOrCIDR
		return r.compileIPOrCIDR(s)
	case strings.HasPrefix(s, regexpCapturePrefix):
		r.kind = kindRegexpCapture
		return r.compileRegexpCapture(s[len(regexpCapturePrefix):])
	case strings.HasPrefix(s, regexpPrefix):
		r.kind = kindRegexp
		return r.compileRegexp(s[len(regexpPrefix):])
	default:
		r.kind = kindExact
		return nil
	}
}

func (r *MatchRule) compileRegexp(pattern string) error {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("invalid regexp %q: %w", pattern, err)
	}
	r.compiledRegexp = re
	return nil
}

func (r *MatchRule) compileRegexpCapture(pattern string) error {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("invalid regexp_capture %q: %w", pattern, err)
	}
	if re.NumSubexp() < 1 {
		return fmt.Errorf("regexp_capture %q has no capturing group; "+
			"wrap the part you want as the bucket key in (...)", pattern)
	}
	r.compiledRegexp = re
	return nil
}

func (r *MatchRule) compileIPOrCIDR(spec string) error {
	if _, ipnet, err := net.ParseCIDR(spec); err == nil {
		r.compiledCIDR = ipnet
		return nil
	}
	ip := net.ParseIP(spec)
	if ip == nil {
		return fmt.Errorf("invalid ip or cidr: %q", spec)
	}
	bits := 32
	if ip.To4() == nil {
		bits = 128
	}
	_, ipnet, err := net.ParseCIDR(fmt.Sprintf("%s/%d", ip.String(), bits))
	if err != nil {
		return fmt.Errorf("failed to build CIDR from %q: %w", spec, err)
	}
	r.compiledCIDR = ipnet
	return nil
}

// Match extracts this rule's value from the given proxy-wasm-style headers
// slice and decides whether it matches.
//
// Return value:
//   - nil      no match (value absent, or present but does not satisfy the pattern)
//   - non-nil  match; *value is the raw extracted string and can be used as part
//              of the Redis key
//
// Value extraction contract per Source:
//   - header / ip            looked up in headers by Name (case insensitive)
//   - param                  parsed from the query string of the ":path" pseudo header
//   - cookie                 parsed from the "cookie" header by Name
//   - consumer               always read from the "x-mse-consumer" header; Name is ignored
func (r *MatchRule) Match(headers [][2]string) *string {
	raw, ok := r.extract(headers)
	if !ok {
		return nil
	}
	var hit bool
	switch r.kind {
	case kindWildcard:
		hit = true
	case kindExact:
		hit = string(r.Value) == raw
	case kindRegexp:
		hit = r.compiledRegexp != nil && r.compiledRegexp.MatchString(raw)
	case kindRegexpCapture:
		// On hit, return the FIRST capture group (m[1]) instead of the raw
		// extracted value. Two requests whose extracted values differ only in
		// a non-captured portion (e.g. the access_key prefix on x-mse-consumer)
		// will produce the same key fragment and share the same bucket.
		if r.compiledRegexp != nil {
			if m := r.compiledRegexp.FindStringSubmatch(raw); m != nil {
				captured := m[1]
				return &captured
			}
		}
		return nil
	case kindIPOrCIDR:
		if r.compiledCIDR != nil {
			ip := net.ParseIP(raw)
			hit = ip != nil && r.compiledCIDR.Contains(ip)
		}
	default:
		// kindUncompiled or unknown: Compile was not called / failed; treat as miss
		return nil
	}
	if !hit {
		return nil
	}
	return &raw
}

// KeyPart returns this rule's fragment of the Redis key: <source>:<name>=<value>.
// The consumer source ignores Name; an empty Name degrades to <source>=<value>.
func (r *MatchRule) KeyPart(value string) string {
	if r.Source == SourceConsumer || r.Name == "" {
		return string(r.Source) + "=" + value
	}
	return string(r.Source) + ":" + r.Name + "=" + value
}

// extract reads the raw string value for this rule's Source / Name.
func (r *MatchRule) extract(headers [][2]string) (string, bool) {
	switch r.Source {
	case SourceHeader, SourceIP:
		return findHeader(headers, r.Name)
	case SourceConsumer:
		return findHeader(headers, ConsumerHeader)
	case SourceParam:
		return extractParam(headers, r.Name)
	case SourceCookie:
		return extractCookie(headers, r.Name)
	}
	return "", false
}

// findHeader looks up name in headers using case-insensitive comparison.
func findHeader(headers [][2]string, name string) (string, bool) {
	if name == "" {
		return "", false
	}
	for _, kv := range headers {
		if strings.EqualFold(kv[0], name) {
			return kv[1], true
		}
	}
	return "", false
}

// extractParam parses a single query parameter from the ":path" pseudo header.
func extractParam(headers [][2]string, name string) (string, bool) {
	if name == "" {
		return "", false
	}
	path, ok := findHeader(headers, ":path")
	if !ok {
		return "", false
	}
	_, query, ok := strings.Cut(path, "?")
	if !ok {
		return "", false
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return "", false
	}
	if !values.Has(name) {
		return "", false
	}
	return values.Get(name), true
}

// extractCookie parses a single cookie attribute from the "cookie" header.
func extractCookie(headers [][2]string, name string) (string, bool) {
	if name == "" {
		return "", false
	}
	raw, ok := findHeader(headers, "cookie")
	if !ok {
		return "", false
	}
	for part := range strings.SplitSeq(raw, ";") {
		part = strings.TrimSpace(part)
		key, val, ok := strings.Cut(part, "=")
		if !ok || key == "" {
			continue
		}
		if key == name {
			// RFC 6265: name="value" form; strip the surrounding double quotes.
			if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
				val = val[1 : len(val)-1]
			}
			return val, true
		}
	}
	return "", false
}

// LimitCombination is one rate-limit combination: a set of match rules
// plus optional request-count and token-count quotas.
//
// Each combination supports two flavours of accounting:
//   - Rolling-window (RateQuota) -- "no more than N in any continuous window".
//     Use QueryLimits (requests) or TokenLimits (tokens).
//   - Calendar-aligned token quota (QuotaSpec) -- "no more than N tokens in
//     this calendar day / month / year, resets on the natural boundary in
//     the configured timezone". Use TokenQuota. Request-count is intentionally
//     not offered as a calendar quota: rolling windows already cover the
//     anti-abuse / fairness use cases that "N requests per day" addresses.
//
// Flavours can be combined: e.g. a rolling per-minute rate limit on top of a
// monthly token quota. Each non-nil field contributes a separate Redis key.
type LimitCombination struct {
	// Name uniquely identifies this combination (used in logs, metrics, and
	// the Redis key).
	Name string `json:"name"`

	// Match is the list of dimension match rules; every rule must hit for the
	// combination to be enabled.
	Match []MatchRule `json:"match"`

	// QueryLimits is the rolling-window request-count quota (requests per window).
	QueryLimits *RateQuota `json:"query_limits,omitempty"`

	// TokenLimits is the rolling-window token-count quota (tokens per window).
	TokenLimits *RateQuota `json:"token_limits,omitempty"`

	// TokenQuota is the calendar-aligned token-count quota.
	TokenQuota *QuotaSpec `json:"token_quota,omitempty"`
}

// PeriodKind enumerates the calendar period a QuotaSpec resets on.
type PeriodKind string

const (
	PeriodEachDay   PeriodKind = "each_day"
	PeriodEachMonth PeriodKind = "each_month"
	PeriodEachYear  PeriodKind = "each_year"
)

// defaultTimezone is the IANA timezone used when QuotaSpec.Timezone is empty.
const defaultTimezone = "UTC"

// QuotaSpec is a calendar-aligned quota: the maximum amount allowed within a
// fixed calendar period (each day / month / year). The period boundary is
// anchored to the timezone declared at PluginConfig.Timezone (deployment-wide).
//
// Reuse contract with multi_level_limit.lua: the script does not need to know
// "calendar vs rolling" semantics. The plugin computes the dynamic window
// (now - period_start) at request time and passes it as the lua window arg --
// from the script's point of view this is just a longer / shorter sliding
// window. The Redis key fragment uses a stable label (e.g. "each_month") so
// that the key is stable across the period and naturally rotates only when
// the EXPIRE TTL (window*2) collapses near the boundary.
//
// Exactly one of EachDay / EachMonth / EachYear must be set; the int value
// is the limit (count or tokens, depending on which slot uses this spec).
type QuotaSpec struct {
	// EachDay is the limit per calendar day (00:00 - 24:00 in the deployment timezone).
	EachDay *int `json:"each_day,omitempty"`

	// EachMonth is the limit per calendar month (1st 00:00 - last day 24:00 in the deployment timezone).
	EachMonth *int `json:"each_month,omitempty"`

	// EachYear is the limit per calendar year (Jan 1 00:00 - Dec 31 24:00 in the deployment timezone).
	EachYear *int `json:"each_year,omitempty"`

	// Compile artefacts (unexported; not part of JSON).
	kind     PeriodKind
	quota    int
	location *time.Location
}

// Compile validates the spec and binds it to the deployment-wide timezone
// resolved by PluginConfig.Validate. Must be called once during config
// loading; a non-nil error means the spec is invalid and the plugin should
// refuse to start.
func (q *QuotaSpec) Compile(loc *time.Location) error {
	if loc == nil {
		return errors.New("quota_spec: nil location")
	}
	set := 0
	if q.EachDay != nil {
		q.kind = PeriodEachDay
		q.quota = *q.EachDay
		set++
	}
	if q.EachMonth != nil {
		q.kind = PeriodEachMonth
		q.quota = *q.EachMonth
		set++
	}
	if q.EachYear != nil {
		q.kind = PeriodEachYear
		q.quota = *q.EachYear
		set++
	}
	if set == 0 {
		return errors.New("quota_spec: exactly one of each_day/each_month/each_year must be set")
	}
	if set > 1 {
		return errors.New("quota_spec: only one of each_day/each_month/each_year may be set")
	}
	if q.quota <= 0 {
		return fmt.Errorf("quota_spec: limit must be > 0, got %d", q.quota)
	}
	q.location = loc
	return nil
}

// periodStart returns the start instant of the calendar period containing now.
// Compile must have been called.
func (q *QuotaSpec) periodStart(now time.Time) time.Time {
	t := now.In(q.location)
	switch q.kind {
	case PeriodEachDay:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, q.location)
	case PeriodEachMonth:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, q.location)
	case PeriodEachYear:
		return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, q.location)
	}
	return time.Time{}
}

// periodEnd returns the start of the NEXT period (== exclusive end of the
// current period). time.Date normalizes overflow, so e.g. month 13 becomes
// year+1 / month 1, which gives correct boundaries at year rollover.
// Compile must have been called.
func (q *QuotaSpec) periodEnd(now time.Time) time.Time {
	t := now.In(q.location)
	switch q.kind {
	case PeriodEachDay:
		return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, q.location)
	case PeriodEachMonth:
		return time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, q.location)
	case PeriodEachYear:
		return time.Date(t.Year()+1, 1, 1, 0, 0, 0, 0, q.location)
	}
	return time.Time{}
}

// GetWindowAndQuota returns the dynamic window in seconds (now - periodStart)
// and the configured quota. The window is clamped to >= 1 second to keep the
// lua script's ZRANGEBYSCORE range non-degenerate at the period boundary.
// Returns (0, 0) when Compile has not run successfully.
func (q *QuotaSpec) GetWindowAndQuota(now time.Time) (int64, int) {
	if q.location == nil || q.kind == "" {
		return 0, 0
	}
	diff := now.Unix() - q.periodStart(now).Unix()
	if diff < 1 {
		diff = 1
	}
	return diff, q.quota
}

// KeyPart returns the stable Redis key fragment for this spec, e.g.
// "each_month". The deployment timezone is not encoded into the key: it is
// a deployment-wide constant declared at PluginConfig.Timezone. Returns ""
// before Compile.
func (q *QuotaSpec) KeyPart() string {
	if q.kind == "" {
		return ""
	}
	return string(q.kind)
}

// RateQuota is a sliding-window quota: the maximum amount allowed within a
// time window. The structure is neutral and is reused for request-count and
// token-count limits (and any other "N per window" use case).
type RateQuota struct {
	// PerSecond quota per second.
	PerSecond *int `json:"per_second,omitempty"`

	// PerMinute quota per minute.
	PerMinute *int `json:"per_minute,omitempty"`

	// PerHour quota per hour.
	PerHour *int `json:"per_hour,omitempty"`

	// PerDay quota per day.
	PerDay *int `json:"per_day,omitempty"`

	// PerCustom quota for a custom window.
	PerCustom *int `json:"per_custom,omitempty"`

	// CustomWindowSeconds is the size of the custom window in seconds.
	CustomWindowSeconds *int `json:"custom_window_seconds,omitempty"`
}

// ============================================
// Helper methods
// ============================================

// GetWindowAndQuota returns the window size in seconds and the quota from
// this RateQuota. When no window is configured it returns (0, 0); callers
// should check quota <= 0 to decide whether the quota is effective.
func (r *RateQuota) GetWindowAndQuota() (windowSeconds int64, quota int) {
	if r.PerSecond != nil {
		return 1, *r.PerSecond
	}
	if r.PerMinute != nil {
		return 60, *r.PerMinute
	}
	if r.PerHour != nil {
		return 3600, *r.PerHour
	}
	if r.PerDay != nil {
		return 86400, *r.PerDay
	}
	if r.PerCustom != nil && r.CustomWindowSeconds != nil {
		return int64(*r.CustomWindowSeconds), *r.PerCustom
	}
	return 0, 0
}

// KeyPart returns this quota's window fragment of the Redis key, e.g. "60s".
// Returns an empty string when the quota is not configured.
func (r *RateQuota) KeyPart() string {
	win, quota := r.GetWindowAndQuota()
	if quota <= 0 {
		return ""
	}
	return fmt.Sprintf("%ds", win)
}

// Validate performs full configuration validation and pre-compiles every
// MatchRule and QuotaSpec. The top-level Timezone (defaulting to UTC) is
// loaded once and shared by every QuotaSpec under this config. A non-nil
// error carries the first validation failure and the plugin should refuse
// to start.
func (c *PluginConfig) Validate() error {
	if c.RuleName == "" {
		return errors.New("rule_name must not be empty")
	}
	if len(c.LimitCombinations) == 0 {
		return errors.New("limit_combinations must not be empty")
	}
	if c.RejectedCode != 0 && (c.RejectedCode < 100 || c.RejectedCode > 599) {
		return fmt.Errorf("rejected_code %d out of range [100, 599]", c.RejectedCode)
	}
	tz := c.Timezone
	if tz == "" {
		tz = defaultTimezone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return fmt.Errorf("invalid timezone %q: %w", tz, err)
	}
	c.location = loc
	seen := make(map[string]struct{}, len(c.LimitCombinations))
	for i := range c.LimitCombinations {
		combo := &c.LimitCombinations[i]
		if combo.Name == "" {
			return fmt.Errorf("limit_combinations[%d].name must not be empty", i)
		}
		if _, dup := seen[combo.Name]; dup {
			return fmt.Errorf("duplicate limit_combinations.name %q", combo.Name)
		}
		seen[combo.Name] = struct{}{}
		if len(combo.Match) == 0 {
			return fmt.Errorf("limit_combinations[%q].match must not be empty", combo.Name)
		}
		if combo.QueryLimits == nil && combo.TokenLimits == nil && combo.TokenQuota == nil {
			return fmt.Errorf("limit_combinations[%q] must configure at least one of "+
				"query_limits/token_limits/token_quota", combo.Name)
		}
		for j := range combo.Match {
			if err := combo.Match[j].Compile(); err != nil {
				return fmt.Errorf("limit_combinations[%q].match[%d]: %w", combo.Name, j, err)
			}
		}
		if combo.TokenQuota != nil {
			if err := combo.TokenQuota.Compile(c.location); err != nil {
				return fmt.Errorf("limit_combinations[%q].token_quota: %w", combo.Name, err)
			}
		}
	}
	return nil
}

// InitRedisClusterClient reads the "redis" sub-object from the raw config JSON
// and initializes a Redis cluster client on config.
//
// Supported fields:
//   - service_name (required) Redis service FQDN, e.g. a Kubernetes Service or
//                             a static DNS service whose name ends with ".static".
//   - service_port            Service port; defaults to 6379, or 80 for ".static" services.
//   - username                Redis username (optional; Redis 6.0+ with ACL enabled).
//   - password                Redis password (optional).
//   - timeout                 Per-command timeout in milliseconds; defaults to 1000.
//   - database                Redis database index (optional; defaults to 0).
//
// YAML example:
//
//	redis:
//	  service_name: redis.default.svc.cluster.local
//	  service_port: 6379
//	  username: default
//	  password: "********"
//	  timeout: 1000
//	  database: 0
//
// Static DNS example (service_port defaults to 80):
//
//	redis:
//	  service_name: my-redis.static
//	  password: "********"
// initRedisBackend reads the "redis" sub-object from raw and returns a
// redisBackend. The caller must verify that raw.Get("redis") exists before
// calling; this function treats a missing service_name as an error.
func initRedisBackend(raw gjson.Result) (LimitBackend, error) {
	redisConfig := raw.Get("redis")

	serviceName := redisConfig.Get("service_name").String()
	if serviceName == "" {
		return nil, errors.New("redis service_name must not be empty")
	}

	servicePort := int(redisConfig.Get("service_port").Int())
	if servicePort < 0 {
		return nil, fmt.Errorf("redis service_port %d must be >= 0", servicePort)
	}
	if servicePort == 0 {
		if strings.HasSuffix(serviceName, ".static") {
			// use default logic port which is 80 for static service
			servicePort = 80
		} else {
			servicePort = 6379
		}
	}

	username := redisConfig.Get("username").String()
	password := redisConfig.Get("password").String()
	timeout := int(redisConfig.Get("timeout").Int())
	if timeout < 0 {
		return nil, fmt.Errorf("redis timeout %d must be >= 0", timeout)
	}
	if timeout == 0 {
		timeout = 1000
	}

	database := int(redisConfig.Get("database").Int())
	if database < 0 {
		return nil, fmt.Errorf("redis database %d must be >= 0", database)
	}

	client := wrapper.NewRedisClusterClient(wrapper.FQDNCluster{
		FQDN: serviceName,
		Port: int64(servicePort),
	})
	if err := client.Init(username, password, int64(timeout), wrapper.WithDataBase(database)); err != nil {
		return nil, err
	}
	return &redisBackend{client: client}, nil
}
