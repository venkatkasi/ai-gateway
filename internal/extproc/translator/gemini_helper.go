// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package translator

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/url"
	"path"

	"github.com/google/uuid"
	"google.golang.org/genai"

	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
)

const (
	GCPModelPublisherGoogle    = "google"
	GCPModelPublisherAnthropic = "anthropic"
	GCPMethodGenerateContent   = "generateContent"
	GCPMethodRawPredict        = "rawPredict"
	HTTPHeaderKeyContentLength = "Content-Length"
)

// -------------------------------------------------------------
// Request Conversion Helper for OpenAI to GCP Gemini Translator
// -------------------------------------------------------------.

// openAIMessagesToGeminiContents converts OpenAI messages to Gemini Contents and SystemInstruction.
func openAIMessagesToGeminiContents(messages []openai.ChatCompletionMessageParamUnion) ([]genai.Content, *genai.Content, error) {
	var gcpContents []genai.Content
	var systemInstruction *genai.Content
	knownToolCalls := make(map[string]string)
	var gcpParts []*genai.Part

	for _, msgUnion := range messages {
		switch msgUnion.Type {
		case openai.ChatMessageRoleDeveloper:
			msg := msgUnion.Value.(openai.ChatCompletionDeveloperMessageParam)
			inst, err := developerMsgToGeminiParts(msg)
			if err != nil {
				return nil, nil, fmt.Errorf("error converting developer message: %w", err)
			}
			if len(inst) != 0 {
				if systemInstruction == nil {
					systemInstruction = &genai.Content{}
				}
				systemInstruction.Parts = append(systemInstruction.Parts, inst...)
			}
		case openai.ChatMessageRoleSystem:
			msg := msgUnion.Value.(openai.ChatCompletionSystemMessageParam)
			devMsg := systemMsgToDeveloperMsg(msg)
			inst, err := developerMsgToGeminiParts(devMsg)
			if err != nil {
				return nil, nil, fmt.Errorf("error converting developer message: %w", err)
			}
			if len(inst) != 0 {
				if systemInstruction == nil {
					systemInstruction = &genai.Content{}
				}
				systemInstruction.Parts = append(systemInstruction.Parts, inst...)
			}
		case openai.ChatMessageRoleUser:
			msg := msgUnion.Value.(openai.ChatCompletionUserMessageParam)
			parts, err := userMsgToGeminiParts(msg)
			if err != nil {
				return nil, nil, fmt.Errorf("error converting user message: %w", err)
			}
			gcpParts = append(gcpParts, parts...)
		case openai.ChatMessageRoleTool:
			msg := msgUnion.Value.(openai.ChatCompletionToolMessageParam)
			part, err := toolMsgToGeminiParts(msg, knownToolCalls)
			if err != nil {
				return nil, nil, fmt.Errorf("error converting tool message: %w", err)
			}
			gcpParts = append(gcpParts, part)
		case openai.ChatMessageRoleAssistant:
			// Flush any accumulated user/tool parts before assistant.
			if len(gcpParts) > 0 {
				gcpContents = append(gcpContents, genai.Content{Role: genai.RoleUser, Parts: gcpParts})
				gcpParts = nil
			}
			msg := msgUnion.Value.(openai.ChatCompletionAssistantMessageParam)
			assistantParts, toolCalls, err := assistantMsgToGeminiParts(msg)
			if err != nil {
				return nil, nil, fmt.Errorf("error converting assistant message: %w", err)
			}
			for k, v := range toolCalls {
				knownToolCalls[k] = v
			}
			gcpContents = append(gcpContents, genai.Content{Role: genai.RoleModel, Parts: assistantParts})
		default:
			return nil, nil, fmt.Errorf("invalid role in message: %s", msgUnion.Type)
		}
	}

	// If there are any remaining parts after processing all messages, add them as user content.
	if len(gcpParts) > 0 {
		gcpContents = append(gcpContents, genai.Content{Role: genai.RoleUser, Parts: gcpParts})
	}
	return gcpContents, systemInstruction, nil
}

// developerMsgToGeminiParts converts OpenAI developer message to Gemini Content.
func developerMsgToGeminiParts(msg openai.ChatCompletionDeveloperMessageParam) ([]*genai.Part, error) {
	var parts []*genai.Part

	switch contentValue := msg.Content.Value.(type) {
	case string:
		if contentValue != "" {
			parts = append(parts, genai.NewPartFromText(contentValue))
		}
	case []openai.ChatCompletionContentPartTextParam:
		if len(contentValue) > 0 {
			for _, textParam := range contentValue {
				if textParam.Text != "" {
					parts = append(parts, genai.NewPartFromText(textParam.Text))
				}
			}
		}
	default:
		return nil, fmt.Errorf("unsupported content type in developer message: %T", contentValue)

	}
	return parts, nil
}

// userMsgToGeminiParts converts OpenAI user message to Gemini Parts.
func userMsgToGeminiParts(msg openai.ChatCompletionUserMessageParam) ([]*genai.Part, error) {
	var parts []*genai.Part
	switch contentValue := msg.Content.Value.(type) {
	case string:
		if contentValue != "" {
			parts = append(parts, genai.NewPartFromText(contentValue))
		}
	case []openai.ChatCompletionContentPartUserUnionParam:
		for _, content := range contentValue {
			switch {
			case content.TextContent != nil:
				parts = append(parts, genai.NewPartFromText(content.TextContent.Text))
			case content.ImageContent != nil:
				imgURL := content.ImageContent.ImageURL.URL
				if imgURL == "" {
					// If image URL is empty, we skip it.
					continue
				}

				parsedURL, err := url.Parse(imgURL)
				if err != nil {
					return nil, fmt.Errorf("invalid image URL: %w", err)
				}

				if parsedURL.Scheme == "data" {
					mimeType, imgBytes, err := parseDataURI(imgURL)
					if err != nil {
						return nil, fmt.Errorf("failed to parse data URI: %w", err)
					}
					parts = append(parts, genai.NewPartFromBytes(imgBytes, mimeType))
				} else {
					// Identify mimeType based in image url.
					mimeType := mimeTypeImageJPEG // Default to jpeg if unknown.
					if mt := mime.TypeByExtension(path.Ext(imgURL)); mt != "" {
						mimeType = mt
					}

					parts = append(parts, genai.NewPartFromURI(imgURL, mimeType))
				}
			case content.InputAudioContent != nil:
				// Audio content is currently not supported in this implementation.
				return nil, fmt.Errorf("audio content not supported yet")
			}
		}
	default:
		return nil, fmt.Errorf("unsupported content type in user message: %T", contentValue)
	}
	return parts, nil
}

// toolMsgToGeminiParts converts OpenAI tool message to Gemini Parts.
func toolMsgToGeminiParts(msg openai.ChatCompletionToolMessageParam, knownToolCalls map[string]string) (*genai.Part, error) {
	var part *genai.Part
	name := knownToolCalls[msg.ToolCallID]
	funcResponse := ""
	switch contentValue := msg.Content.Value.(type) {
	case string:
		funcResponse = contentValue
	case []openai.ChatCompletionContentPartTextParam:
		for _, textParam := range contentValue {
			if textParam.Text != "" {
				funcResponse += textParam.Text
			}
		}
	default:
		return nil, fmt.Errorf("unsupported content type in tool message: %T", contentValue)
	}

	part = genai.NewPartFromFunctionResponse(name, map[string]any{"output": funcResponse})
	return part, nil
}

// assistantMsgToGeminiParts converts OpenAI assistant message to Gemini Parts and known tool calls.
func assistantMsgToGeminiParts(msg openai.ChatCompletionAssistantMessageParam) ([]*genai.Part, map[string]string, error) {
	var parts []*genai.Part

	// Handle tool calls in the assistant message.
	knownToolCalls := make(map[string]string)
	for _, toolCall := range msg.ToolCalls {
		knownToolCalls[toolCall.ID] = toolCall.Function.Name
		var parsedArgs map[string]any
		if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &parsedArgs); err != nil {
			return nil, nil, fmt.Errorf("function arguments should be valid json string. failed to parse function arguments: %w", err)
		}
		parts = append(parts, genai.NewPartFromFunctionCall(toolCall.Function.Name, parsedArgs))
	}

	// Handle content in the assistant message.
	switch v := msg.Content.Value.(type) {
	case string:
		if v != "" {
			parts = append(parts, genai.NewPartFromText(v))
		}
	case []openai.ChatCompletionAssistantMessageParamContent:
		for _, contPart := range v {
			switch contPart.Type {
			case openai.ChatCompletionAssistantMessageParamContentTypeText:
				if contPart.Text != nil && *contPart.Text != "" {
					parts = append(parts, genai.NewPartFromText(*contPart.Text))
				}
			case openai.ChatCompletionAssistantMessageParamContentTypeRefusal:
				// Refusal messages are currently ignored in this implementation.
			default:
				return nil, nil, fmt.Errorf("unsupported content type in assistant message: %s", contPart.Type)
			}
		}
	case nil:
		// No content provided, this is valid.
	default:
		return nil, nil, fmt.Errorf("unsupported content type in assistant message: %T", v)
	}

	return parts, knownToolCalls, nil
}

// openAIToolsToGeminiTools converts OpenAI tools to Gemini tools.
// This function combines all the openai tools into a single Gemini Tool as distinct function declarations.
// This is mainly done because some Gemini models do not support multiple tools in a single request.
// This behavior might need to change in future based on model capabilities.
// Example Input
// [
//
//	{
//	  "type": "function",
//	  "function": {
//	    "name": "add",
//	    "description": "Add two numbers",
//	    "parameters": {
//	      "properties": {
//	        "a": {
//	          "type": "integer"
//	        },
//	        "b": {
//	          "type": "integer"
//	        }
//	      },
//	      "required": [
//	        "a",
//	        "b"
//	      ],
//	      "type": "object"
//	    }
//	  }
//	}
//
// ]
//
// Example Output
// [
//
//	{
//	  "functionDeclarations": [
//	    {
//	      "description": "Add two numbers",
//	      "name": "add",
//	      "parametersJsonSchema": {
//	        "properties": {
//	          "a": {
//	            "type": "integer"
//	          },
//	          "b": {
//	            "type": "integer"
//	          }
//	        },
//	        "required": [
//	          "a",
//	          "b"
//	        ],
//	        "type": "object"
//	      }
//	    }
//	  ]
//	}
//
// ].
func openAIToolsToGeminiTools(openaiTools []openai.Tool) ([]genai.Tool, error) {
	if len(openaiTools) == 0 {
		return nil, nil
	}
	var functionDecls []*genai.FunctionDeclaration
	for _, tool := range openaiTools {
		if tool.Type == openai.ToolTypeFunction {
			if tool.Function != nil {
				functionDecl := &genai.FunctionDeclaration{
					Name:                 tool.Function.Name,
					Description:          tool.Function.Description,
					ParametersJsonSchema: tool.Function.Parameters,
				}
				functionDecls = append(functionDecls, functionDecl)
			}
		}
	}
	if len(functionDecls) == 0 {
		return nil, nil
	}
	return []genai.Tool{{FunctionDeclarations: functionDecls}}, nil
}

// openAIToolChoiceToGeminiToolConfig converts OpenAI tool_choice to Gemini ToolConfig.
// Example Input
//
//	{
//	 "type": "function",
//	 "function": {
//	   "name": "myfunc"
//	 }
//	}
//
// Example Output
//
//	{
//	 "functionCallingConfig": {
//	   "mode": "ANY",
//	   "allowedFunctionNames": [
//	     "myfunc"
//	   ]
//	 }
//	}
func openAIToolChoiceToGeminiToolConfig(toolChoice interface{}) (*genai.ToolConfig, error) {
	if toolChoice == nil {
		return nil, nil
	}
	switch tc := toolChoice.(type) {
	case string:
		switch tc {
		case "auto":
			return &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAuto}}, nil
		case "none":
			return &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeNone}}, nil
		case "required":
			return &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny}}, nil
		default:
			return nil, fmt.Errorf("unsupported tool choice: '%s'", tc)
		}
	case openai.ToolChoice:
		return &genai.ToolConfig{
			FunctionCallingConfig: &genai.FunctionCallingConfig{
				Mode:                 genai.FunctionCallingConfigModeAny,
				AllowedFunctionNames: []string{tc.Function.Name},
			},
			RetrievalConfig: nil,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported tool choice type: %T", toolChoice)
	}
}

// openAIReqToGeminiGenerationConfig converts OpenAI request to Gemini GenerationConfig.
func openAIReqToGeminiGenerationConfig(openAIReq *openai.ChatCompletionRequest) (*genai.GenerationConfig, error) {
	gc := &genai.GenerationConfig{}
	if openAIReq.Temperature != nil {
		f := float32(*openAIReq.Temperature)
		gc.Temperature = &f
	}
	if openAIReq.TopP != nil {
		f := float32(*openAIReq.TopP)
		gc.TopP = &f
	}

	if openAIReq.Seed != nil {
		seed := int32(*openAIReq.Seed) // nolint:gosec
		gc.Seed = &seed
	}

	if openAIReq.TopLogProbs != nil {
		logProbs := int32(*openAIReq.TopLogProbs) // nolint:gosec
		gc.Logprobs = &logProbs
	}

	if openAIReq.LogProbs != nil {
		gc.ResponseLogprobs = *openAIReq.LogProbs
	}

	if openAIReq.ResponseFormat != nil {
		switch openAIReq.ResponseFormat.Type {
		case openai.ChatCompletionResponseFormatTypeText:
			gc.ResponseMIMEType = mimeTypeTextPlain
		case openai.ChatCompletionResponseFormatTypeJSONObject:
			gc.ResponseMIMEType = mimeTypeApplicationJSON
		case openai.ChatCompletionResponseFormatTypeJSONSchema:
			var schemaMap map[string]interface{}

			switch sch := openAIReq.ResponseFormat.JSONSchema.Schema.(type) {
			case string:
				if err := json.Unmarshal([]byte(sch), &schemaMap); err != nil {
					return nil, fmt.Errorf("invalid JSON schema string: %w", err)
				}
			case map[string]interface{}:
				schemaMap = sch
			}

			gc.ResponseMIMEType = mimeTypeApplicationJSON
			gc.ResponseJsonSchema = schemaMap
		}
	}

	if openAIReq.N != nil {
		gc.CandidateCount = int32(*openAIReq.N) // nolint:gosec
	}
	if openAIReq.MaxTokens != nil {
		gc.MaxOutputTokens = int32(*openAIReq.MaxTokens) // nolint:gosec
	}
	if openAIReq.PresencePenalty != nil {
		gc.PresencePenalty = openAIReq.PresencePenalty
	}
	if openAIReq.FrequencyPenalty != nil {
		gc.FrequencyPenalty = openAIReq.FrequencyPenalty
	}
	stopSeq, err := processStop(openAIReq.Stop)
	if err != nil {
		return nil, err
	}
	if len(stopSeq) > 0 {
		var stops []string
		for _, s := range stopSeq {
			if s != nil {
				stops = append(stops, *s)
			}
		}
		gc.StopSequences = stops
	}
	return gc, nil
}

// --------------------------------------------------------------
// Response Conversion Helper for GCP Gemini to OpenAI Translator
// --------------------------------------------------------------.

// geminiCandidatesToOpenAIChoices converts Gemini candidates to OpenAI choices.
func geminiCandidatesToOpenAIChoices(candidates []*genai.Candidate) ([]openai.ChatCompletionResponseChoice, error) {
	choices := make([]openai.ChatCompletionResponseChoice, 0, len(candidates))

	for idx, candidate := range candidates {
		if candidate == nil {
			continue
		}

		// Create the choice.
		choice := openai.ChatCompletionResponseChoice{
			Index:        int64(idx),
			FinishReason: geminiFinishReasonToOpenAI(candidate.FinishReason),
		}

		if candidate.Content != nil {
			message := openai.ChatCompletionResponseChoiceMessage{
				Role: openai.ChatMessageRoleAssistant,
			}
			// Extract text from parts.
			content := extractTextFromGeminiParts(candidate.Content.Parts)
			message.Content = &content

			// Extract tool calls if any.
			toolCalls, err := extractToolCallsFromGeminiParts(candidate.Content.Parts)
			if err != nil {
				return nil, fmt.Errorf("error extracting tool calls: %w", err)
			}
			message.ToolCalls = toolCalls

			// If there's no content but there are tool calls, set content to nil.
			if content == "" && len(toolCalls) > 0 {
				message.Content = nil
			}

			choice.Message = message
		}

		// Handle logprobs if available.
		if candidate.LogprobsResult != nil {
			choice.Logprobs = geminiLogprobsToOpenAILogprobs(*candidate.LogprobsResult)
		}

		choices = append(choices, choice)
	}

	return choices, nil
}

// geminiFinishReasonToOpenAI converts Gemini finish reason to OpenAI finish reason.
func geminiFinishReasonToOpenAI(reason genai.FinishReason) openai.ChatCompletionChoicesFinishReason {
	switch reason {
	case genai.FinishReasonStop:
		return openai.ChatCompletionChoicesFinishReasonStop
	case genai.FinishReasonMaxTokens:
		return openai.ChatCompletionChoicesFinishReasonLength
	default:
		return openai.ChatCompletionChoicesFinishReasonContentFilter
	}
}

// extractTextFromGeminiParts extracts text from Gemini parts.
func extractTextFromGeminiParts(parts []*genai.Part) string {
	var text string
	for _, part := range parts {
		if part != nil && part.Text != "" {
			text += part.Text
		}
	}
	return text
}

// extractToolCallsFromGeminiParts extracts tool calls from Gemini parts.
func extractToolCallsFromGeminiParts(parts []*genai.Part) ([]openai.ChatCompletionMessageToolCallParam, error) {
	var toolCalls []openai.ChatCompletionMessageToolCallParam

	for _, part := range parts {
		if part == nil || part.FunctionCall == nil {
			continue
		}

		// Convert function call arguments to JSON string.
		args, err := json.Marshal(part.FunctionCall.Args)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal function arguments: %w", err)
		}

		// Generate a random ID for the tool call.
		toolCallID := uuid.New().String()

		toolCall := openai.ChatCompletionMessageToolCallParam{
			ID:   toolCallID,
			Type: "function",
			Function: openai.ChatCompletionMessageToolCallFunctionParam{
				Name:      part.FunctionCall.Name,
				Arguments: string(args),
			},
		}

		toolCalls = append(toolCalls, toolCall)
	}

	if len(toolCalls) == 0 {
		return nil, nil
	}

	return toolCalls, nil
}

// geminiUsageToOpenAIUsage converts Gemini usage metadata to OpenAI usage.
func geminiUsageToOpenAIUsage(metadata *genai.GenerateContentResponseUsageMetadata) openai.ChatCompletionResponseUsage {
	if metadata == nil {
		return openai.ChatCompletionResponseUsage{}
	}

	return openai.ChatCompletionResponseUsage{
		CompletionTokens: int(metadata.CandidatesTokenCount),
		PromptTokens:     int(metadata.PromptTokenCount),
		TotalTokens:      int(metadata.TotalTokenCount),
	}
}

// geminiLogprobsToOpenAILogprobs converts Gemini logprobs to OpenAI logprobs.
func geminiLogprobsToOpenAILogprobs(logprobsResult genai.LogprobsResult) openai.ChatCompletionChoicesLogprobs {
	if len(logprobsResult.ChosenCandidates) == 0 {
		return openai.ChatCompletionChoicesLogprobs{}
	}

	content := make([]openai.ChatCompletionTokenLogprob, 0, len(logprobsResult.ChosenCandidates))

	for i := 0; i < len(logprobsResult.ChosenCandidates); i++ {
		chosen := logprobsResult.ChosenCandidates[i]

		var topLogprobs []openai.ChatCompletionTokenLogprobTopLogprob

		// Process top candidates if available.
		if i < len(logprobsResult.TopCandidates) && logprobsResult.TopCandidates[i] != nil {
			topCandidates := logprobsResult.TopCandidates[i].Candidates
			if len(topCandidates) > 0 {
				topLogprobs = make([]openai.ChatCompletionTokenLogprobTopLogprob, 0, len(topCandidates))
				for _, tc := range topCandidates {
					topLogprobs = append(topLogprobs, openai.ChatCompletionTokenLogprobTopLogprob{
						Token:   tc.Token,
						Logprob: float64(tc.LogProbability),
					})
				}
			}
		}

		// Create token logprob.
		tokenLogprob := openai.ChatCompletionTokenLogprob{
			Token:       chosen.Token,
			Logprob:     float64(chosen.LogProbability),
			TopLogprobs: topLogprobs,
		}

		content = append(content, tokenLogprob)
	}

	// Return the logprobs.
	return openai.ChatCompletionChoicesLogprobs{
		Content: content,
	}
}

func buildGCPModelPathSuffix(publisher, model, gcpMethod string) string {
	pathSuffix := fmt.Sprintf("publishers/%s/models/%s:%s", publisher, model, gcpMethod)
	return pathSuffix
}
