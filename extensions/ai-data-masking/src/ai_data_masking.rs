// Copyright (c) 2025 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

use crate::deny_word::DenyWord;
use crate::msg_win_openai::MsgWindow;
use fancy_regex::Regex;
use grok::patterns;
use higress_wasm_rust::log::Log;
use higress_wasm_rust::plugin_wrapper::{HttpContextWrapper, RootContextWrapper};
use higress_wasm_rust::request_wrapper::has_request_body;
use higress_wasm_rust::rule_matcher::{on_configure, RuleMatcher, SharedRuleMatcher};
use jsonpath_rust::{JsonPath, JsonPathValue};
use lazy_static::lazy_static;
use proxy_wasm::traits::{Context, HttpContext, RootContext};
use proxy_wasm::types::{Bytes, ContextType, DataAction, HeaderAction, LogLevel};
use serde::de::Error;
use serde::Deserialize;
use serde::Deserializer;
use serde_json::{json, Value};
use std::cell::RefCell;
use std::collections::{BTreeMap, HashMap, VecDeque};
use std::fmt::Write;
use std::ops::DerefMut;
use std::rc::{Rc, Weak};
use std::str::FromStr;
use std::vec;

proxy_wasm::main! {{
    proxy_wasm::set_log_level(LogLevel::Trace);
    proxy_wasm::set_root_context(|_|Box::new(AiDataMaskingRoot::new()));
}}

const PLUGIN_NAME: &str = "ai-data-masking";
const GROK_PATTERN: &str = r"%\{(?<name>(?<pattern>[A-z0-9]+)(?::(?<alias>[A-z0-9_:;\/\s\.]+))?)\}";

struct System {
    deny_word: DenyWord,
    grok_regex: Regex,
    grok_patterns: BTreeMap<String, String>,
}
lazy_static! {
    static ref SYSTEM: System = System::new();
}

struct AiDataMaskingRoot {
    log: Log,
    rule_matcher: SharedRuleMatcher<AiDataMaskingConfig>,
}
struct AiDataMasking {
    weak: Weak<RefCell<Box<dyn HttpContextWrapper<AiDataMaskingConfig>>>>,
    config: Option<Rc<AiDataMaskingConfig>>,
    mask_map: HashMap<String, Option<String>>,
    is_openai: bool,
    is_openai_stream: Option<bool>,
    // latched once a mid-stream deny terminated the response; subsequent
    // body callbacks must be suppressed so buffered text cannot leak
    denied: bool,
    stream: bool,
    log: Log,
    msg_window: MsgWindow,
    char_window_size: usize,
    byte_window_size: usize,
}
fn deserialize_regexp<'de, D>(deserializer: D) -> Result<Regex, D::Error>
where
    D: Deserializer<'de>,
{
    let value: Value = Deserialize::deserialize(deserializer)?;
    if let Some(pattern) = value.as_str() {
        let (p, _) = SYSTEM.grok_to_pattern(pattern);
        if let Ok(reg) = Regex::new(&p) {
            Ok(reg)
        } else if let Ok(reg) = Regex::new(pattern) {
            Ok(reg)
        } else {
            Err(Error::custom(format!("regexp error field {}", pattern)))
        }
    } else {
        Err(Error::custom("regexp error not string".to_string()))
    }
}

fn deserialize_type<'de, D>(deserializer: D) -> Result<Type, D::Error>
where
    D: Deserializer<'de>,
{
    let value: Value = Deserialize::deserialize(deserializer)?;
    if let Some(t) = value.as_str() {
        if t == "replace" {
            Ok(Type::Replace)
        } else if t == "hash" {
            Ok(Type::Hash)
        } else {
            Err(Error::custom(format!("regexp error value {}", t)))
        }
    } else {
        Err(Error::custom("type error not string".to_string()))
    }
}

fn deserialize_denyword<'de, D>(deserializer: D) -> Result<DenyWord, D::Error>
where
    D: Deserializer<'de>,
{
    let value: Vec<String> = Deserialize::deserialize(deserializer)?;
    Ok(DenyWord::from_iter(value))
}

fn deserialize_jsonpath<'de, D>(deserializer: D) -> Result<Vec<JsonPath>, D::Error>
where
    D: Deserializer<'de>,
{
    let value: Vec<String> = Deserialize::deserialize(deserializer)?;
    let mut ret = Vec::new();
    for v in value {
        if v.is_empty() {
            continue;
        }
        match JsonPath::from_str(&v) {
            Ok(jp) => ret.push(jp),
            Err(_) => return Err(Error::custom(format!("jsonpath error value {}", v))),
        }
    }
    Ok(ret)
}

#[derive(Debug, Clone, PartialEq, Eq)]
enum Type {
    Replace,
    Hash,
}

#[derive(Debug, Deserialize, Clone)]
struct Rule {
    #[serde(deserialize_with = "deserialize_regexp")]
    regex: Regex,
    #[serde(deserialize_with = "deserialize_type", alias = "type")]
    type_: Type,
    #[serde(default)]
    restore: bool,
    #[serde(default)]
    value: String,
}
fn default_deny_openai() -> bool {
    true
}
fn default_deny_raw() -> bool {
    false
}
fn default_system_deny() -> bool {
    false
}
fn default_deny_code() -> u16 {
    200
}
fn default_deny_content_type() -> String {
    "application/json".to_string()
}
fn default_deny_raw_message() -> String {
    "{\"errmsg\":\"提问或回答中包含敏感词，已被屏蔽\"}".to_string()
}
fn default_deny_message() -> String {
    "提问或回答中包含敏感词，已被屏蔽".to_string()
}
#[derive(Default, Debug, Deserialize, Clone)]
pub struct AiDataMaskingConfig {
    #[serde(default = "default_deny_openai")]
    deny_openai: bool,
    #[serde(default = "default_deny_raw")]
    deny_raw: bool,
    #[serde(default, deserialize_with = "deserialize_jsonpath")]
    deny_jsonpath: Vec<JsonPath>,
    #[serde(default = "default_system_deny")]
    system_deny: bool,
    #[serde(default = "default_deny_code")]
    deny_code: u16,
    #[serde(default = "default_deny_message")]
    deny_message: String,
    #[serde(default = "default_deny_raw_message")]
    deny_raw_message: String,
    #[serde(default = "default_deny_content_type")]
    deny_content_type: String,
    #[serde(default)]
    replace_roles: Vec<Rule>,
    #[serde(deserialize_with = "deserialize_denyword", default = "DenyWord::empty")]
    deny_words: DenyWord,
}

impl AiDataMaskingConfig {
    fn check_message(&self, message: &str, log: &Log) -> bool {
        if let Some(word) = self.deny_words.check(message) {
            log.warn(&format!(
                "custom deny word {} matched from {}",
                word, message
            ));
            return true;
        } else if self.system_deny {
            if let Some(word) = SYSTEM.deny_word.check(message) {
                log.warn(&format!(
                    "system deny word {} matched from {}",
                    word, message
                ));
                return true;
            }
        }
        false
    }
}
// NOTE: the request/response bodies are processed via serde_json::Value
// instead of strict typed structs on purpose. Real-world OpenAI traffic
// contains shapes that would fail a strict `content: String`:
//   - multimodal `content: [{"type": "text", "text": "..."}]`
//   - assistant tool-call turns with an explicit `"content": null`
// A strict struct deserialization failure used to silently skip the whole
// deny/masking pipeline (see gpustack-enterprise#399), so every field is now
// handled dynamically: null is ignored, arrays are scanned per text part,
// and tool call arguments are checked/masked as strings.

static SYSTEM_PATTERNS: &[(&str, &str)] = &[
    ("MOBILE", r#"\d{8,11}"#),
    ("IDCARD", r#"\d{17}[0-9xX]|\d{15}"#),
];

impl System {
    fn new() -> Self {
        let grok_regex = Regex::new(GROK_PATTERN).unwrap();
        let grok_patterns = BTreeMap::new();
        let mut system = System {
            deny_word: DenyWord::system(),
            grok_regex,
            grok_patterns,
        };
        system.init();
        system
    }
    fn init(&mut self) {
        let mut grok_temp_patterns = VecDeque::new();
        for patterns in [patterns(), SYSTEM_PATTERNS] {
            for &(key, value) in patterns {
                if self.grok_regex.is_match(value).is_ok_and(|r| r) {
                    grok_temp_patterns.push_back((String::from(key), String::from(value)));
                } else {
                    self.grok_patterns
                        .insert(String::from(key), String::from(value));
                }
            }
        }
        let mut last_ok: Option<String> = None;

        while let Some((key, value)) = grok_temp_patterns.pop_front() {
            if let Some(k) = &last_ok {
                if k == &key {
                    break;
                }
            }
            let (v, ok) = self.grok_to_pattern(&value);
            if ok {
                self.grok_patterns.insert(key, v);
                last_ok = None;
            } else {
                if last_ok.is_none() {
                    last_ok = Some(key.clone());
                }
                grok_temp_patterns.push_back((key, v));
            }
        }
    }
    fn grok_to_pattern(&self, pattern: &str) -> (String, bool) {
        let mut ok = true;
        let mut ret = pattern.to_string();
        for capture in self.grok_regex.captures_iter(pattern) {
            if capture.is_err() {
                ok = false;
                continue;
            }
            let c = capture.unwrap();
            if let (Some(full), Some(name)) = (c.get(0), c.name("pattern")) {
                if let Some(p) = self.grok_patterns.get(name.as_str()) {
                    if let Some(alias) = c.name("alias") {
                        ret = ret.replace(full.as_str(), &format!("(?P<{}>{})", alias.as_str(), p));
                    } else {
                        ret = ret.replace(full.as_str(), p);
                    }
                } else {
                    ok = false;
                }
            }
        }
        (ret, ok)
    }
}

impl AiDataMaskingRoot {
    fn new() -> Self {
        AiDataMaskingRoot {
            log: Log::new(PLUGIN_NAME.to_string()),
            rule_matcher: Rc::new(RefCell::new(RuleMatcher::default())),
        }
    }
}

impl Context for AiDataMaskingRoot {}

impl RootContext for AiDataMaskingRoot {
    fn on_configure(&mut self, plugin_configuration_size: usize) -> bool {
        on_configure(
            self,
            plugin_configuration_size,
            self.rule_matcher.borrow_mut().deref_mut(),
            &self.log,
        )
    }
    fn create_http_context(&self, context_id: u32) -> Option<Box<dyn HttpContext>> {
        self.create_http_context_use_wrapper(context_id)
    }
    fn get_type(&self) -> Option<ContextType> {
        Some(ContextType::HttpContext)
    }
}

impl RootContextWrapper<AiDataMaskingConfig> for AiDataMaskingRoot {
    fn rule_matcher(&self) -> &SharedRuleMatcher<AiDataMaskingConfig> {
        &self.rule_matcher
    }

    fn create_http_context_wrapper(
        &self,
        _context_id: u32,
    ) -> Option<Box<dyn HttpContextWrapper<AiDataMaskingConfig>>> {
        Some(Box::new(AiDataMasking {
            weak: Weak::default(),
            mask_map: HashMap::new(),
            config: None,
            is_openai: false,
            is_openai_stream: None,
            denied: false,
            stream: false,
            msg_window: MsgWindow::default(),
            log: Log::new(PLUGIN_NAME.to_string()),
            char_window_size: 0,
            byte_window_size: 0,
        }))
    }
}

impl AiDataMasking {
    fn check_message(&self, message: &str) -> bool {
        if let Some(config) = &self.config {
            config.check_message(message, self.log())
        } else {
            false
        }
    }
    fn msg_to_response(&self, msg: &str, raw_msg: &str, content_type: &str) -> (String, String) {
        if !self.is_openai {
            (raw_msg.to_string(), content_type.to_string())
        } else if self.stream {
            (
                // emit the deny message as a chunk with finish_reason set, then
                // terminate the SSE stream properly with [DONE]
                format!(
                    "data: {0}\n\ndata: {1}\n\ndata: [DONE]\n\n",
                    json!({"choices": [{"index": 0, "delta": {"role": "assistant", "content": msg}}], "usage": {}}),
                    json!({"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "usage": {}})
                ),
                "text/event-stream;charset=UTF-8".to_string(),
            )
        } else {
            (
                json!({"choices": [{"index": 0, "message": {"role": "assistant", "content": msg}}], "usage": {}}).to_string(),
                 "application/json".to_string()
            )
        }
    }
    fn deny(&mut self, in_response: bool) -> DataAction {
        if in_response && self.stream {
            // latch the denied state so subsequent body callbacks (including
            // the end-of-stream flush) cannot emit buffered text after the
            // terminator we send here
            self.denied = true;
            // mid-stream deny: emit a proper SSE termination (finish_reason +
            if self.is_openai_stream == Some(true) {
                let body = format!(
                    "data: {0}\n\ndata: [DONE]\n\n",
                    json!({"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "usage": {}})
                );
                self.replace_http_response_body(body.as_bytes());
            } else {
                self.replace_http_response_body(&[]);
            }
            return DataAction::Continue;
        }
        let (deny_code, (deny_message, content_type)) = if let Some(config) = &self.config {
            (
                config.deny_code,
                self.msg_to_response(
                    &config.deny_message,
                    &config.deny_raw_message,
                    &config.deny_content_type,
                ),
            )
        } else {
            (
                default_deny_code(),
                self.msg_to_response(
                    &default_deny_message(),
                    &default_deny_raw_message(),
                    &default_deny_content_type(),
                ),
            )
        };
        if in_response {
            self.replace_http_response_body(deny_message.as_bytes());
            return DataAction::Continue;
        }
        self.send_http_response(
            deny_code as u32,
            vec![("Content-Type", &content_type)],
            Some(deny_message.as_bytes()),
        );
        DataAction::StopIterationAndBuffer
    }

    fn replace_request_msg(&mut self, message: &str) -> String {
        let config = self.config.as_ref().unwrap();
        let mut msg = message.to_string();
        for rule in &config.replace_roles {
            let mut replace_pair = Vec::new();
            if rule.type_ == Type::Replace && !rule.restore {
                msg = rule.regex.replace_all(&msg, &rule.value).to_string();
            } else {
                for mc in rule.regex.find_iter(&msg) {
                    if mc.is_err() {
                        continue;
                    }
                    let m = mc.unwrap();
                    let from_word = m.as_str();

                    let to_word = match rule.type_ {
                        Type::Hash => {
                            let digest = hmac_sha256::Hash::hash(from_word.as_bytes());
                            digest.iter().fold(String::new(), |mut output, b| {
                                let _ = write!(output, "{b:02x}");
                                output
                            })
                        }
                        Type::Replace => rule.regex.replace(from_word, &rule.value).to_string(),
                    };
                    if to_word.len() > self.byte_window_size {
                        self.byte_window_size = to_word.len();
                    }
                    if to_word.chars().count() > self.char_window_size {
                        self.char_window_size = to_word.chars().count();
                    }

                    replace_pair.push((from_word.to_string(), to_word.clone()));

                    if rule.restore && !to_word.is_empty() {
                        match self.mask_map.entry(to_word) {
                            std::collections::hash_map::Entry::Occupied(mut e) => {
                                e.insert(None);
                            }
                            std::collections::hash_map::Entry::Vacant(e) => {
                                e.insert(Some(from_word.to_string()));
                            }
                        }
                    }
                }
                for (from_word, to_word) in replace_pair {
                    msg = msg.replace(&from_word, &to_word);
                }
            }
        }
        if msg != message {
            self.log()
                .debug(&format!("replace_request_msg from {} to {}", message, msg));
        }
        msg
    }

    /// Deny-check and mask one JSON string field using the request-side
    /// replace rules. Non-string values (e.g. explicit nulls) are ignored.
    /// Returns true when the value matched a deny word.
    fn mask_request_string(&mut self, value: &mut Value) -> bool {
        let Some(s) = value.as_str().map(|s| s.to_string()) else {
            return false;
        };
        if self.check_message(&s) {
            return true;
        }
        let new_s = self.replace_request_msg(&s);
        if new_s != s {
            *value = Value::String(new_s);
        }
        false
    }

    /// Deny-check and mask one JSON string field using the response-side
    /// mask map (placeholder restore). Returns true on a deny word match.
    fn mask_response_string(&self, value: &mut Value) -> bool {
        let Some(s) = value.as_str().map(|s| s.to_string()) else {
            return false;
        };
        if self.check_message(&s) {
            return true;
        }
        if !self.mask_map.is_empty() {
            let mut new_s = s.clone();
            for (from_word, to_word) in self.mask_map.iter() {
                if let Some(to) = to_word {
                    new_s = new_s.replace(from_word, to);
                }
            }
            if new_s != s {
                *value = Value::String(new_s);
            }
        }
        false
    }

    /// Recursively traverse a JSON value and deny-check / mask every string
    /// found inside (objects, arrays, nested combinations). Used for
    /// JSON-encoded fields such as tool_calls arguments, where operating on
    /// the serialized string directly would both miss values that are escaped
    /// inside the JSON and risk producing invalid JSON when the replacement
    /// itself needs escaping.
    fn mask_json_value(&mut self, value: &mut Value, request_side: bool) -> bool {
        match value {
            Value::String(_) => {
                if request_side {
                    self.mask_request_string(value)
                } else {
                    self.mask_response_string(value)
                }
            }
            Value::Array(arr) => {
                for v in arr.iter_mut() {
                    if self.mask_json_value(v, request_side) {
                        return true;
                    }
                }
                false
            }
            Value::Object(obj) => {
                for v in obj.values_mut() {
                    if self.mask_json_value(v, request_side) {
                        return true;
                    }
                }
                false
            }
            _ => false,
        }
    }

    /// Deny-check and mask a JSON-encoded string field (tool_calls
    /// arguments): parse it, mask every string value recursively, serialize
    /// back. Falls back to plain string masking when the payload is not
    /// valid JSON (e.g. fragmented or free-form arguments).
    fn mask_json_string_field(&mut self, value: &mut Value, request_side: bool) -> bool {
        let Some(s) = value.as_str().map(|s| s.to_string()) else {
            return false;
        };
        match serde_json::from_str::<Value>(&s) {
            Ok(mut parsed) => {
                if self.mask_json_value(&mut parsed, request_side) {
                    return true;
                }
                let new_s = parsed.to_string();
                if new_s != s {
                    *value = Value::String(new_s);
                }
                false
            }
            Err(_) => {
                if request_side {
                    self.mask_request_string(value)
                } else {
                    self.mask_response_string(value)
                }
            }
        }
    }

    /// Deny-check and mask one chat message Value, tolerating every shape
    /// seen in real traffic: `content` as a string, an array of parts
    /// (multimodal), or null; optional reasoning fields; assistant
    /// `tool_calls[].function.arguments` (a JSON-encoded string that can
    /// carry sensitive data, e.g. agent-injected parameters).
    /// Returns true when the message must be denied.
    fn mask_message_value(&mut self, message: &mut Value, request_side: bool) -> bool {
        let mask_one = |this: &mut Self, v: &mut Value| -> bool {
            if request_side {
                this.mask_request_string(v)
            } else {
                this.mask_response_string(v)
            }
        };
        if let Some(content) = message.get_mut("content") {
            match content {
                Value::String(_) => {
                    if mask_one(self, content) {
                        return true;
                    }
                }
                Value::Array(parts) => {
                    for part in parts.iter_mut() {
                        if let Some(text) = part.get_mut("text") {
                            if mask_one(self, text) {
                                return true;
                            }
                        }
                    }
                }
                _ => {}
            }
        }
        for field in ["reasoning_content", "reasoning"] {
            if let Some(v) = message.get_mut(field) {
                if mask_one(self, v) {
                    return true;
                }
            }
        }
        if let Some(tool_calls) = message.get_mut("tool_calls").and_then(|t| t.as_array_mut()) {
            for tc in tool_calls.iter_mut() {
                if let Some(args) = tc.get_mut("function").and_then(|f| f.get_mut("arguments")) {
                    if self.mask_json_string_field(args, request_side) {
                        return true;
                    }
                }
            }
        }
        false
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // proxy-wasm hostcalls only exist inside the wasm host; stub the ones
    // referenced by logging paths so unit tests can link and run natively
    #[no_mangle]
    pub extern "C" fn proxy_log(_level: u32, _msg: *const u8, _size: usize) {}

    fn masking_with_deny_words(words: &[&str]) -> AiDataMasking {
        let config = AiDataMaskingConfig {
            deny_words: DenyWord::from_iter(words.iter().map(|w| w.to_string())),
            ..Default::default()
        };
        AiDataMasking {
            weak: Weak::new(),
            config: Some(Rc::new(config)),
            mask_map: HashMap::new(),
            is_openai: false,
            is_openai_stream: None,
            denied: false,
            stream: false,
            log: Log::new(PLUGIN_NAME.to_string()),
            msg_window: MsgWindow::default(),
            char_window_size: 0,
            byte_window_size: 0,
        }
    }

    // issue gpustack-enterprise#399 step 2: array (multimodal) content must
    // no longer bypass the deny pipeline
    #[test]
    fn test_deny_array_content() {
        let mut m = masking_with_deny_words(&["SkUnTest1112"]);
        let mut msg = json!({"role": "user", "content": [
            {"type": "text", "text": "Repeat this token verbatim: SkUnTest1112"}
        ]});
        assert!(m.mask_message_value(&mut msg, true));
    }

    // issue step 3: explicit "content": null on an assistant tool-call turn
    // must no longer fail the whole parse; the deny word in the tool message
    // is still caught
    #[test]
    fn test_deny_null_content_tool_calls() {
        let mut m = masking_with_deny_words(&["SkUnTest1112"]);
        let mut msg = json!({"role": "assistant", "content": null,
            "tool_calls": [{"id": "call_1", "type": "function",
                "function": {"name": "lookup", "arguments": "{\"q\": \"ident\"}"}}]});
        // the null content itself carries nothing, parse must survive
        assert!(!m.mask_message_value(&mut msg, true));
        // a deny word inside tool call arguments is caught (live evidence:
        // agent-injected parameters leak through tool_calls)
        let mut msg = json!({"role": "assistant", "content": null,
            "tool_calls": [{"id": "call_1", "type": "function",
                "function": {"name": "lookup", "arguments": "{\"q\": \"SkUnTest1112\"}"}}]});
        assert!(m.mask_message_value(&mut msg, true));
    }

    // string content in tool message still caught (issue step 4 control)
    #[test]
    fn test_deny_tool_message_content() {
        let mut m = masking_with_deny_words(&["SkUnTest1112"]);
        let mut msg = json!({"role": "tool", "tool_call_id": "call_1",
            "content": "result: SkUnTest1112"});
        assert!(m.mask_message_value(&mut msg, true));
    }

    // clean messages must pass through untouched, including image parts
    #[test]
    fn test_clean_multimodal_passes() {
        let mut m = masking_with_deny_words(&["SkUnTest1112"]);
        let mut msg = json!({"role": "user", "content": [
            {"type": "text", "text": "hello"},
            {"type": "image_url", "image_url": {"url": "http://example.com/a.png"}}
        ]});
        let before = msg.clone();
        assert!(!m.mask_message_value(&mut msg, true));
        assert_eq!(msg, before);
    }

    // response side with "content": null must also parse and be checked
    #[test]
    fn test_response_null_content() {
        let mut m = masking_with_deny_words(&["SkUnTest1112"]);
        let mut msg = json!({"role": "assistant", "content": null,
            "reasoning": "thinking about SkUnTest1112"});
        assert!(m.mask_message_value(&mut msg, false));
    }

    // recursive masking inside JSON-encoded tool arguments: values needing
    // JSON escaping must survive masking round-trip as valid JSON (review
    // feedback on PR #59: raw-string replacement could corrupt the JSON)
    #[test]
    fn test_tool_call_arguments_json_recursive_masking() {
        let mut m = masking_with_deny_words(&["SkUnTest1112"]);
        // deny word nested inside the arguments JSON
        let mut msg = json!({"role": "assistant", "content": null,
            "tool_calls": [{"id": "call_1", "type": "function",
                "function": {"name": "lookup",
                    "arguments": "{\"q\": \"a SkUnTest1112 b\", \"nested\": {\"list\": [\"x\"]}}"}}]});
        assert!(m.mask_message_value(&mut msg, true));

        // masking (not deny) inside arguments, incl. values that need
        // escaping in JSON: the result must still parse as valid JSON
        let mut m = masking_with_deny_words(&[]);
        m.mask_map.insert("MARKER-7735".to_string(), Some("quote\"and\\slash".to_string()));
        let mut msg = json!({"role": "assistant", "content": null,
            "tool_calls": [{"id": "call_1", "type": "function",
                "function": {"name": "lookup",
                    "arguments": "{\"q\": \"MARKER-7735\"}"}}]});
        assert!(!m.mask_message_value(&mut msg, false));
        let args = msg["tool_calls"][0]["function"]["arguments"].as_str().unwrap();
        let parsed: Value = serde_json::from_str(args).expect("arguments stay valid JSON");
        assert_eq!(parsed["q"].as_str(), Some("quote\"and\\slash"));

        // arguments that are not valid JSON fall back to plain string masking
        let mut m = masking_with_deny_words(&["SkUnTest1112"]);
        let mut msg = json!({"role": "assistant", "content": null,
            "tool_calls": [{"id": "call_1", "type": "function",
                "function": {"name": "lookup", "arguments": "raw SkUnTest1112 text"}}]});
        assert!(m.mask_message_value(&mut msg, true));
    }

    // streamed tool_calls argument fragments are accumulated across chunks;
    // a deny word split across fragments must still be caught in the
    // assembled arguments (review feedback on PR #59)
    #[test]
    fn test_streamed_tool_args_accumulation() {
        use crate::msg_win_openai::MsgWindow;
        let mut msg_win = MsgWindow::default();
        let chunks = [
            r#"data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":""}}]}}]}"#,
            r#"data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\": \"SkUn"}}]}}]}"#,
            r#"data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"Test1112\"}"}}]}}]}"#,
            r#"data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}"#,
            "data: [DONE]",
        ];
        for c in chunks {
            msg_win.push(c.as_bytes(), true);
            msg_win.push(b"\n\n", true);
            let _ = msg_win.pop(7, 7, true);
        }
        let _ = msg_win.finish(true);
        let assembled: Vec<&String> = msg_win.tool_args().collect();
        assert_eq!(assembled, vec![&"{\"q\": \"SkUnTest1112\"}".to_string()]);
    }

    // issue #399 comment (tool_calls blind spot) part 1: a restore rule masks
    // the value request-side into a placeholder, the model echoes the
    // placeholder into tool arguments; the response side must restore the
    // original there too, or the client executes the tool with the placeholder
    #[test]
    fn test_response_restores_tool_call_arguments() {
        let mut m = masking_with_deny_words(&["SkUnTest1112"]);
        m.mask_map.insert("<MASKED-ID>".to_string(), Some("MARKER-7735".to_string()));
        let mut msg = json!({"role": "assistant", "content": null,
            "tool_calls": [{"id": "call_1", "type": "function",
                "function": {"name": "get_weather", "arguments": "{\"city\": \"<MASKED-ID>\"}"}}]});
        assert!(!m.mask_message_value(&mut msg, false));
        let args = msg["tool_calls"][0]["function"]["arguments"]
            .as_str()
            .unwrap();
        // arguments are re-serialized compactly, but the restored value must
        // survive intact and stay valid JSON
        let parsed: Value = serde_json::from_str(args).expect("arguments stay valid JSON");
        assert_eq!(parsed["city"].as_str(), Some("MARKER-7735"));
        // function name must never be touched: it is an identifier, not data
        assert_eq!(
            msg["tool_calls"][0]["function"]["name"].as_str(),
            Some("get_weather")
        );
    }
}

impl Context for AiDataMasking {}

impl HttpContext for AiDataMasking {
    fn on_http_request_headers(
        &mut self,
        _num_headers: usize,
        _end_of_stream: bool,
    ) -> HeaderAction {
        if has_request_body() {
            self.set_http_request_header("Content-Length", None);
            HeaderAction::StopIteration
        } else {
            HeaderAction::Continue
        }
    }
    fn on_http_response_headers(
        &mut self,
        _num_headers: usize,
        _end_of_stream: bool,
    ) -> HeaderAction {
        self.set_http_response_header("Content-Length", None);
        HeaderAction::Continue
    }

    fn on_http_response_body(&mut self, body_size: usize, end_of_stream: bool) -> DataAction {
        if !self.stream {
            return DataAction::Continue;
        }
        if self.denied {
            // stream already terminated by deny; drop everything else,
            // including the end-of-stream flush, so no buffered text can
            // be emitted after the terminator
            self.replace_http_response_body(&[]);
            return DataAction::Continue;
        }
        if body_size > 0 {
            if let Some(body) = self.get_http_response_body(0, body_size) {
                if self.is_openai && self.is_openai_stream.is_none() {
                    self.is_openai_stream = Some(body.starts_with(b"data:"));
                }
                self.msg_window
                    .push(&body, self.is_openai_stream.unwrap_or_default());
                let mut deny = false;
                let log = Log::new(PLUGIN_NAME.to_string());
                for message in self.msg_window.messages_iter_mut() {
                    if let Ok(mut msg) = String::from_utf8(message.clone()) {
                        if let Some(config) = &self.config {
                            if config.check_message(&msg, &log) {
                                deny = true;
                                break;
                            }
                        }
                        if !self.mask_map.is_empty() {
                            for (from_word, to_word) in self.mask_map.iter() {
                                if let Some(to) = to_word {
                                    msg = msg.replace(from_word, to);
                                }
                            }
                        }
                        message.clear();
                        message.extend_from_slice(msg.as_bytes());
                    }
                }
                // streamed tool_calls argument fragments are re-emitted
                // untouched (rewriting split JSON fragments would corrupt the
                // tool call), but the assembled arguments are deny-checked
                // and terminate the stream on a match
                if !deny {
                    if let Some(config) = &self.config {
                        for args in self.msg_window.tool_args() {
                            if !args.is_empty() && config.check_message(args, &log) {
                                deny = true;
                                break;
                            }
                        }
                    }
                }
                if deny {
                    return self.deny(true);
                }
            }
        }
        let is_openai_stream = self.is_openai_stream.unwrap_or(false);
        let new_body = if end_of_stream {
            self.msg_window.finish(is_openai_stream)
        } else {
            self.msg_window.pop(
                self.char_window_size * 2,
                self.byte_window_size * 2,
                is_openai_stream,
            )
        };
        self.replace_http_response_body(&new_body);
        DataAction::Continue
    }
}

impl HttpContextWrapper<AiDataMaskingConfig> for AiDataMasking {
    fn init_self_weak(
        &mut self,
        self_weak: std::rc::Weak<RefCell<Box<dyn HttpContextWrapper<AiDataMaskingConfig>>>>,
    ) {
        self.weak = self_weak;
    }
    fn log(&self) -> &Log {
        &self.log
    }
    fn on_config(&mut self, config: Rc<AiDataMaskingConfig>) {
        self.config = Some(config.clone());
    }
    fn cache_request_body(&self) -> bool {
        true
    }
    fn cache_response_body(&self) -> bool {
        !self.stream
    }
    fn on_http_request_complete_body(&mut self, req_body: &Bytes) -> DataAction {
        if self.config.is_none() {
            return DataAction::Continue;
        }
        let config = self.config.as_ref().unwrap();
        let mut req_body = match serde_json::from_slice::<Value>(req_body) {
            Ok(r) => r.to_string(),
            Err(_) => {
                if let Ok(r) = String::from_utf8(req_body.clone()) {
                    r
                } else {
                    return DataAction::Continue;
                }
            }
        };
        if config.deny_openai {
            match serde_json::from_str::<Value>(req_body.as_str()) {
                Ok(mut json)
                    if json.get("messages").is_some_and(|m| m.is_array()) =>
                {
                    self.is_openai = true;
                    if let Some(stream) = json.get("stream").and_then(|s| s.as_bool()) {
                        self.stream = stream;
                    }
                    let mut denied = false;
                    if let Some(messages) = json.get_mut("messages").and_then(|m| m.as_array_mut())
                    {
                        for msg in messages.iter_mut() {
                            if self.mask_message_value(msg, true) {
                                denied = true;
                                break;
                            }
                        }
                    }
                    if denied {
                        return self.deny(false);
                    }
                    self.replace_http_request_body(json.to_string().as_bytes());
                    return DataAction::Continue;
                }
                Ok(_) => {
                    // valid JSON but not an OpenAI chat body; fall through to
                    // the jsonpath / raw branches below
                }
                Err(_) => {
                    // never fail silently: an unparseable body must be visible
                    // in the logs so operators can spot a bypass attempt
                    self.log().warn(
                        "openai request body is not valid JSON, deny/masking skipped for it",
                    );
                }
            }
        }
        if !config.deny_jsonpath.is_empty() {
            if let Ok(json) = serde_json::from_str::<Value>(req_body.as_str()) {
                for jsonpath in config.deny_jsonpath.clone() {
                    for v in jsonpath.find_slice(&json) {
                        if let JsonPathValue::Slice(d, _) = v {
                            if let Some(s) = d.as_str() {
                                if self.check_message(s) {
                                    return self.deny(false);
                                }
                                let content = s.to_string();
                                let new_content = self.replace_request_msg(&content);
                                if new_content != content {
                                    req_body = req_body.replace(
                                        &Value::String(content).to_string(),
                                        &Value::String(new_content).to_string(),
                                    );
                                }
                            }
                        }
                    }
                }
                self.replace_http_request_body(req_body.as_bytes());
                return DataAction::Continue;
            }
        }
        if config.deny_raw {
            if self.check_message(&req_body) {
                return self.deny(false);
            }
            let new_body = self.replace_request_msg(&req_body);
            if new_body != req_body {
                self.replace_http_request_body(new_body.as_bytes())
            }
            return DataAction::Continue;
        }
        DataAction::Continue
    }
    fn on_http_response_complete_body(&mut self, res_body: &Bytes) -> DataAction {
        if self.config.is_none() {
            return DataAction::Continue;
        }
        let config = self.config.as_ref().unwrap();
        let mut res_body = match serde_json::from_slice::<Value>(res_body) {
            Ok(r) => r.to_string(),
            Err(_) => {
                if let Ok(r) = String::from_utf8(res_body.clone()) {
                    r
                } else {
                    return DataAction::Continue;
                }
            }
        };
        if config.deny_openai && self.is_openai {
            if let Ok(mut json) = serde_json::from_str::<Value>(res_body.as_str()) {
                let mut denied = false;
                if let Some(choices) = json.get_mut("choices").and_then(|c| c.as_array_mut()) {
                    for choice in choices.iter_mut() {
                        if let Some(message) = choice.get_mut("message") {
                            if self.mask_message_value(message, false) {
                                denied = true;
                                break;
                            }
                        }
                    }
                }
                if denied {
                    return self.deny(true);
                }
                self.replace_http_response_body(json.to_string().as_bytes());
                return DataAction::Continue;
            }
        }
        if config.deny_raw {
            if self.check_message(&res_body) {
                return self.deny(true);
            }
            if !self.mask_map.is_empty() {
                for (from_word, to_word) in self.mask_map.iter() {
                    if let Some(to) = to_word {
                        res_body = res_body.replace(from_word, to);
                    }
                }
            }
            self.replace_http_response_body(res_body.as_bytes());
            return DataAction::Continue;
        }
        DataAction::Continue
    }
}
