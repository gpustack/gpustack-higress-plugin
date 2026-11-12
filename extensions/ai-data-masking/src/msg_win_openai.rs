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

use std::collections::{HashMap, VecDeque};

use higress_wasm_rust::event_stream::EventStream;
use serde::Deserialize;
use serde_json::Value;

use crate::msg_window::MessageWindow;
use crate::number_merge::NumberMerge;

#[derive(PartialEq, Eq, Clone, Copy)]
enum MsgFlag {
    None,
    Content,
    ReasoningContent,
    Reasoning,
}
impl Default for MsgFlag {
    fn default() -> Self {
        Self::None
    }
}
#[derive(Deserialize)]
struct Delta {
    #[serde(default)]
    content: Option<String>,
    #[serde(default)]
    reasoning_content: Option<String>,
    // vLLM >= 0.30 exposes chain-of-thought in delta.reasoning
    #[serde(default)]
    reasoning: Option<String>,
}
#[derive(Deserialize)]
struct Choices {
    #[serde(default)]
    index: i64,
    #[serde(default)]
    delta: Option<Delta>,
    #[serde(default)]
    finish_reason: Option<String>,
}

impl Delta {
    /// Iterate over every populated text field in this delta. A chunk may
    /// carry both content and reasoning (e.g. at the reasoning-to-content
    /// transition), so callers must process each one instead of picking one.
    fn fields(&self) -> Vec<(MsgFlag, &str)> {
        let mut ret = Vec::new();
        if let Some(msg) = &self.content {
            if !msg.is_empty() {
                ret.push((MsgFlag::Content, msg.as_str()));
            }
        }
        if let Some(msg) = &self.reasoning_content {
            if !msg.is_empty() {
                ret.push((MsgFlag::ReasoningContent, msg.as_str()));
            }
        }
        if let Some(msg) = &self.reasoning {
            if !msg.is_empty() {
                ret.push((MsgFlag::Reasoning, msg.as_str()));
            }
        }
        ret
    }
}
const USAGE_PATH: &str = "usage";
const CHOICES_PATH: &str = "choices";

type MessageLine = Vec<(MsgFlag, Vec<u8>)>;

#[derive(Default)]
struct MessageWindowOpenAi {
    message_window: MessageWindow,
    ret_messages: MessageLine,
    flag: MsgFlag,
    last_value: Value,
    finish_reason: Option<String>,
    // raw chunks carrying structured payloads (tool_calls / function_call /
    // audio) queued for re-emission untouched, in arrival order
    passthrough: VecDeque<Value>,
}

/// Structured delta payloads are not text: masking rules must not apply and
/// fragments must be re-emitted as-is, or function calling breaks.
fn is_passthrough_delta(delta: &Value) -> bool {
    delta.as_object().is_some_and(|o| {
        o.contains_key("tool_calls") || o.contains_key("function_call") || o.contains_key("audio")
    })
}

impl MessageWindowOpenAi {
    /// Capture per-choice metadata. Called for every choice chunk, even
    /// terminal ones (`delta: null` or empty) that carry no text, so that
    /// finish_reason (stop/length/tool_calls/...) is never lost.
    fn capture(&mut self, value: &Value, finish_reason: &Option<String>) {
        self.last_value = value.clone();
        if let Some(fr) = finish_reason {
            self.finish_reason = Some(fr.clone());
        }
    }

    fn update(&mut self, data: &[u8], flag: MsgFlag) {
        if data.is_empty() {
            return;
        }
        if self.flag == MsgFlag::None {
            self.flag = flag;
        }
        if self.flag != flag {
            let last_flag = core::mem::replace(&mut self.flag, flag);
            let msg = self.message_window.finish();
            self.ret_messages.push((last_flag, msg));
        }
        self.message_window.update(data);
    }

    fn push_passthrough(&mut self, choice_value: Value) {
        self.passthrough.push_back(choice_value);
    }

    fn gen_value(&self, flag: &MsgFlag, msg: &[u8], finish: bool) -> Value {
        let mut ret = self.last_value.clone();
        match flag {
            MsgFlag::Content => {
                ret["delta"]["content"] = Value::String(String::from_utf8_lossy(msg).to_string());
                if let Some(m) = ret["delta"].as_object_mut() {
                    m.remove("reasoning_content");
                    m.remove("reasoning");
                }
            }
            MsgFlag::ReasoningContent => {
                ret["delta"]["reasoning_content"] =
                    Value::String(String::from_utf8_lossy(msg).to_string());
                ret["delta"]["content"] = Value::String(String::new());
                if let Some(m) = ret["delta"].as_object_mut() {
                    m.remove("reasoning");
                }
            }
            MsgFlag::Reasoning => {
                ret["delta"]["reasoning"] =
                    Value::String(String::from_utf8_lossy(msg).to_string());
                ret["delta"]["content"] = Value::String(String::new());
                if let Some(m) = ret["delta"].as_object_mut() {
                    m.remove("reasoning_content");
                }
            }
            _ => {}
        }
        if finish {
            ret["finish_reason"] = self
                .finish_reason
                .as_ref()
                .map_or(Value::Null, |v| Value::String(v.to_string()));
        } else {
            ret["finish_reason"] = Value::Null;
        }
        ret
    }

    fn messages_to_value(&mut self) -> Vec<Value> {
        let mut ret = Vec::new();
        for (flag, msg) in core::mem::take(&mut self.ret_messages) {
            ret.push(self.gen_value(&flag, &msg, false));
        }
        ret
    }

    fn pop(&mut self, char_window_size: usize, byte_window_size: usize) -> Vec<Value> {
        let mut ret = self.messages_to_value();
        // re-emit queued structured payloads (tool_calls fragments etc.)
        // untouched, preserving arrival order among themselves
        ret.extend(self.passthrough.drain(..));

        let msg = self.message_window.pop(char_window_size, byte_window_size);
        if !msg.is_empty() {
            ret.push(self.gen_value(&self.flag, &msg, false));
        }

        ret
    }
    fn finish(&mut self) -> Vec<Value> {
        let mut ret = self.messages_to_value();
        // structured payloads first, then the terminal chunk carrying
        // finish_reason (stop/length/tool_calls/...)
        ret.extend(self.passthrough.drain(..));
        let msg = self.message_window.finish();
        let flag = core::mem::replace(&mut self.flag, MsgFlag::None);
        ret.push(self.gen_value(&flag, &msg, true));

        ret
    }
    fn iter_mut(&mut self) -> impl Iterator<Item = &mut Vec<u8>> {
        self.ret_messages
            .iter_mut()
            .map(|(_, msg)| msg)
            .chain(self.message_window.iter_mut())
    }
}

#[derive(Default)]
pub(crate) struct MsgWindow {
    stream_parser: EventStream,
    base_message_window: MessageWindow,
    message_windows: HashMap<i64, MessageWindowOpenAi>,
    last_value: Value,
    usage: NumberMerge,
}

impl MsgWindow {
    fn update_event(&mut self, event: Vec<u8>) -> Option<Vec<u8>> {
        if event.is_empty() || !event.starts_with(b"data:") {
            Some(event)
        } else if let Ok(res) = serde_json::from_slice::<Value>(&event[b"data:".len()..]) {
            self.last_value = res;
            if let Some(r) = self.last_value.as_object() {
                if let Some(v) = r.get(USAGE_PATH) {
                    self.usage.add(v);
                }
                if let Some(v) = r.get(CHOICES_PATH) {
                    if let Some(a) = v.as_array() {
                        for item in a {
                            if let Ok(c) = serde_json::from_value::<Choices>(item.clone()) {
                                let mw = self.message_windows.entry(c.index).or_default();
                                // capture terminal metadata even when the
                                // choice carries no text (delta null/empty)
                                mw.capture(item, &c.finish_reason);
                                // structured payloads are queued for
                                // untouched re-emission instead of being fed
                                // into the masking text window
                                if item.get("delta").is_some_and(is_passthrough_delta) {
                                    mw.push_passthrough(item.clone());
                                }
                                if let Some(d) = &c.delta {
                                    for (flag, msg) in d.fields() {
                                        mw.update(msg.as_bytes(), flag);
                                    }
                                }
                            }
                        }
                    }
                }
            }
            None
        } else if event.starts_with(b"data: [DONE]") || event.starts_with(b"data:[DONE]") {
            None
        } else {
            Some(event)
        }
    }
    fn push_base(&mut self, data: &[u8]) {
        self.base_message_window.update(data);
    }
    pub(crate) fn push(&mut self, data: &[u8], is_openai: bool) {
        if is_openai {
            self.stream_parser.update(data.to_vec());
            while let Some(event) = self.stream_parser.next() {
                if let Some(msg) = self.update_event(event) {
                    self.push_base(&msg);
                }
            }
        } else {
            self.push_base(data);
        }
    }

    pub(crate) fn pop(
        &mut self,
        char_window_size: usize,
        byte_window_size: usize,
        is_openai: bool,
    ) -> Vec<u8> {
        if !is_openai {
            return self
                .base_message_window
                .pop(char_window_size, byte_window_size);
        }
        let mut ret = Vec::new();
        for mw in self.message_windows.values_mut() {
            for value in mw.pop(char_window_size, byte_window_size) {
                let usage = self.usage.finish();
                let mut ret_value = self.last_value.clone();
                ret_value[CHOICES_PATH] = Value::Array(vec![value]);
                ret_value[USAGE_PATH] = usage;
                ret.extend(format!("data: {}\n\n", ret_value).as_bytes())
            }
        }
        ret
    }
    pub(crate) fn finish(&mut self, is_openai: bool) -> Vec<u8> {
        if !is_openai {
            return self.base_message_window.finish();
        }
        if let Some(event) = self.stream_parser.flush() {
            self.update_event(event);
        }
        let mut ret = Vec::new();
        for mw in &mut self.message_windows.values_mut() {
            for value in mw.finish() {
                let usage = self.usage.finish();
                let mut ret_value = self.last_value.clone();
                ret_value[CHOICES_PATH] = Value::Array(vec![value]);
                ret_value[USAGE_PATH] = usage;
                ret.extend(format!("data: {}\n\n", ret_value).as_bytes())
            }
        }
        // upstream [DONE] events are consumed (and dropped) in update_event,
        // so terminate the reconstructed stream explicitly
        ret.extend(b"data: [DONE]\n\n");
        ret
    }
    pub(crate) fn messages_iter_mut(&mut self) -> impl Iterator<Item = &mut Vec<u8>> {
        self.base_message_window.iter_mut().chain(
            self.message_windows
                .values_mut()
                .flat_map(|mw| mw.iter_mut()),
        )
    }
}

#[cfg(test)]
mod tests {

    use rust_embed::Embed;

    use super::*;
    #[derive(Embed)]
    #[folder = "test/"]
    struct Asset;
    #[derive(Deserialize)]
    struct Res {
        choices: Vec<Choices>,
    }

    impl Res {
        fn get_text(&self) -> (String, String, String) {
            let mut content = String::new();
            let mut reasoning_content = String::new();
            let mut reasoning = String::new();
            for choice in self.choices.iter() {
                if let Some(delta) = &choice.delta {
                    if let Some(c) = &delta.content {
                        content += c;
                    }
                    if let Some(rc) = &delta.reasoning_content {
                        reasoning_content += rc;
                    }
                    if let Some(r) = &delta.reasoning {
                        reasoning += r;
                    }
                }
            }
            (content, reasoning_content, reasoning)
        }
    }

    fn run_masking_roundtrip(file_name: &str, use_reasoning_field: bool) {
        let mut msg_win = MsgWindow::default();
        let data = raw_message(file_name);
        let mut buffer = Vec::new();
        for line in data.split("\n") {
            msg_win.push(line.as_bytes(), true);
            msg_win.push(b"\n\n", true);
            for message in msg_win.messages_iter_mut() {
                if let Ok(mut msg) = String::from_utf8(message.clone()) {
                    msg = msg.replace("Higress", "***higress***");
                    message.clear();
                    message.extend_from_slice(msg.as_bytes());
                }
            }

            buffer.extend(msg_win.pop(7, 7, true));
        }
        buffer.extend(msg_win.finish(true));
        let mut message = String::new();
        let mut reasoning_message = String::new();
        let mut reasoning_field_message = String::new();
        let mut saw_done = false;
        for line in buffer.split(|&x| x == b'\n') {
            if line.is_empty() {
                continue;
            }
            assert!(line.starts_with(b"data:"));
            if line.starts_with(b"data: [DONE]") {
                saw_done = true;
                continue;
            }
            let des = serde_json::from_slice::<Res>(&line[b"data:".len()..]);
            assert!(des.is_ok());
            let res = des.unwrap();
            let (c, rc, r) = res.get_text();
            message.push_str(&c);
            reasoning_message.push_str(&rc);
            reasoning_field_message.push_str(&r);
        }
        assert!(saw_done, "stream must be terminated with [DONE]");
        let res = "***higress*** 是一个基于 Istio 的高性能服务网格数据平面项目，旨在提供高吞吐量、低延迟和可扩展的服务通信管理。它为企业级应用提供了丰富的流量治理功能，如负载均衡、熔断、限流等，并支持多协议代理（包括 HTTP/1.1, HTTP/2, gRPC）。***higress*** 的设计目标是优化 Istio 在大规模集群中的性能表现，满足高并发场景下的需求。";
        assert_eq!(message, res);
        // the chain-of-thought roundtrips through whichever field the upstream
        // uses (reasoning_content for qwen2.5/ollama, reasoning for vLLM >= 0.30)
        if use_reasoning_field {
            assert_eq!(reasoning_field_message, res);
            assert_eq!(reasoning_message, "");
        } else {
            assert_eq!(reasoning_message, res);
            assert_eq!(reasoning_field_message, "");
        }
    }

    #[test]
    fn test_msg() {
        // qwen2.5 / ollama style: delta.reasoning_content
        run_masking_roundtrip("raw_message.txt", false);
    }

    #[test]
    fn test_msg_reasoning_field() {
        // vLLM >= 0.30 style: delta.reasoning
        run_masking_roundtrip("raw_message_reasoning.txt", true);
    }

    #[test]
    fn test_msg_tool_calls() {
        // function calling: delta.tool_calls fragments must be re-emitted
        // untouched (id/name/arguments pieces in order) and the terminal
        // empty-delta chunk must keep finish_reason = "tool_calls"
        let mut msg_win = MsgWindow::default();
        let data = raw_message("raw_message_tool_calls.txt");
        let mut buffer = Vec::new();
        for line in data.split("\n") {
            msg_win.push(line.as_bytes(), true);
            msg_win.push(b"\n\n", true);
            buffer.extend(msg_win.pop(7, 7, true));
        }
        buffer.extend(msg_win.finish(true));

        let mut tool_call_ids = Vec::new();
        let mut function_names = Vec::new();
        let mut arguments = String::new();
        let mut finish_reasons = Vec::new();
        let mut saw_done = false;
        for line in buffer.split(|&x| x == b'\n') {
            if line.is_empty() {
                continue;
            }
            assert!(line.starts_with(b"data:"));
            if line.starts_with(b"data: [DONE]") {
                saw_done = true;
                continue;
            }
            let value: Value =
                serde_json::from_slice(&line[b"data:".len()..]).expect("valid json chunk");
            if let Some(choices) = value.get("choices").and_then(|c| c.as_array()) {
                for choice in choices {
                    if let Some(fr) = choice.get("finish_reason") {
                        if !fr.is_null() {
                            finish_reasons.push(fr.as_str().unwrap_or("").to_string());
                        }
                    }
                    if let Some(tcs) = choice
                        .get("delta")
                        .and_then(|d| d.get("tool_calls"))
                        .and_then(|t| t.as_array())
                    {
                        for tc in tcs {
                            if let Some(id) =
                                tc.get("id").and_then(|v| v.as_str())
                            {
                                tool_call_ids.push(id.to_string());
                            }
                            if let Some(name) = tc
                                .get("function")
                                .and_then(|f| f.get("name"))
                                .and_then(|v| v.as_str())
                            {
                                function_names.push(name.to_string());
                            }
                            if let Some(args) = tc
                                .get("function")
                                .and_then(|f| f.get("arguments"))
                                .and_then(|v| v.as_str())
                            {
                                arguments.push_str(args);
                            }
                        }
                    }
                }
            }
        }
        assert!(saw_done, "stream must be terminated with [DONE]");
        assert_eq!(
            tool_call_ids,
            vec!["call_abc123".to_string()],
            "tool call id must survive"
        );
        assert_eq!(
            function_names,
            vec!["get_weather".to_string()],
            "function name must survive"
        );
        assert_eq!(arguments, "{\"city\": \"北京\"}");
        assert!(
            finish_reasons.contains(&"tool_calls".to_string()),
            "finish_reason tool_calls must be preserved, got {:?}",
            finish_reasons
        );
    }

    fn raw_message(file_name: &str) -> String {
        if let Some(file) = Asset::get(file_name) {
            if let Ok(data) = std::str::from_utf8(file.data.as_ref()) {
                return data.to_string();
            }
        }
        String::new()
    }
}
