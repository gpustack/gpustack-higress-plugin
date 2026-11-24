package provider

import (
	"regexp"
	"strings"

	"github.com/gpustack/gpustack-higress-plugins/extensions/gpustack-ai-proxy/util"
)

// pathPair mirrors the shared inbound classification tables previously owned
// by package main; moved here so providers (openai) can reuse the same
// deterministic classifier instead of re-implementing it against capability
// paths, which would diverge for custom base urls and templated routes.
type pathPair struct {
	key   string
	value ApiName
}

type pathPatternPair struct {
	key   *regexp.Regexp
	value ApiName
}

var (
	pathSuffixToApiName = []pathPair{
		// OpenAI style
		{PathOpenAIChatCompletions, ApiNameChatCompletion},
		{PathOpenAICompletions, ApiNameCompletion},
		{PathOpenAIEmbeddings, ApiNameEmbeddings},
		{PathOpenAIAudioSpeech, ApiNameAudioSpeech},
		{PathOpenAIAudioTranscriptions, ApiNameAudioTranscription},
		{PathOpenAIAudioTranslations, ApiNameAudioTranslation},
		{PathOpenAIRealtime, ApiNameRealtime},
		{PathOpenAIImageGeneration, ApiNameImageGeneration},
		{PathOpenAIImageVariation, ApiNameImageVariation},
		{PathOpenAIImageEdit, ApiNameImageEdit},
		{PathOpenAIBatches, ApiNameBatches},
		{PathOpenAIFiles, ApiNameFiles},
		{PathOpenAIModels, ApiNameModels},
		{PathOpenAIFineTuningJobs, ApiNameFineTuningJobs},
		{PathOpenAIResponses, ApiNameResponses},
		{PathOpenAIVideos, ApiNameVideos},
		// Synchronous generation returns raw video rather than a task object.
		{PathOpenAIVideos + "/sync", ""},
		// Anthropic style
		{PathAnthropicMessagesCountTokens, ApiNameAnthropicCountTokens},
		{PathAnthropicMessages, ApiNameAnthropicMessages},
		{PathAnthropicComplete, ApiNameAnthropicComplete},
		// Cohere style
		{PathCohereV1Rerank, ApiNameCohereV1Rerank},
		// Qwen style
		{PathQwenV1Reranks, ApiNameQwenV1Rerank},
		{PathQwenV1Conversations, ApiNameQwenV1Conversations},
	}
	pathPatternToApiName = []pathPatternPair{
		// OpenAI style
		{util.RegRetrieveBatchPath, ApiNameRetrieveBatch},
		{util.RegCancelBatchPath, ApiNameCancelBatch},
		{util.RegRetrieveFilePath, ApiNameRetrieveFile},
		{util.RegRetrieveFileContentPath, ApiNameRetrieveFileContent},
		{util.RegRetrieveVideoPath, ApiNameRetrieveVideo},
		{util.RegRetrieveVideoContentPath, ApiNameRetrieveVideoContent},
		{util.RegVideoRemixPath, ApiNameVideoRemix},
		{util.RegRetrieveFineTuningJobPath, ApiNameRetrieveFineTuningJob},
		{util.RegRetrieveFineTuningJobEventsPath, ApiNameFineTuningJobEvents},
		{util.RegRetrieveFineTuningJobCheckpointsPath, ApiNameFineTuningJobCheckpoints},
		{util.RegCancelFineTuningJobPath, ApiNameCancelFineTuningJob},
		{util.RegResumeFineTuningJobPath, ApiNameResumeFineTuningJob},
		{util.RegPauseFineTuningJobPath, ApiNamePauseFineTuningJob},
		{util.RegFineTuningCheckpointPermissionPath, ApiNameFineTuningCheckpointPermissions},
		{util.RegDeleteFineTuningCheckpointPermissionPath, ApiNameDeleteFineTuningCheckpointPermission},
		// Gemini style
		{util.RegGeminiGenerateContent, ApiNameGeminiGenerateContent},
		{util.RegGeminiStreamGenerateContent, ApiNameGeminiStreamGenerateContent},
	}
)

// ClassifyPath is the shared, deterministic inbound path classifier: an
// ordered suffix scan followed by an ordered regex scan. It classifies the
// client's request path as sent, independent of any provider's capability
// or custom-url rewriting.
func ClassifyPath(path string) ApiName {
	// Check path suffix matches first
	for _, p := range pathSuffixToApiName {
		if strings.HasSuffix(path, p.key) {
			return p.value
		}
	}

	// Check path pattern matches
	for _, p := range pathPatternToApiName {
		if p.key.MatchString(path) {
			return p.value
		}
	}

	return ""
}
