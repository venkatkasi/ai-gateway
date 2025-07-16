// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extproc

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3http "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/envoyproxy/ai-gateway/filterapi"
	"github.com/envoyproxy/ai-gateway/filterapi/x"
	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
	"github.com/envoyproxy/ai-gateway/internal/extproc/backendauth"
	"github.com/envoyproxy/ai-gateway/internal/extproc/translator"
	"github.com/envoyproxy/ai-gateway/internal/llmcostcel"
)

// ChatCompletionProcessorFactory returns a factory method to instantiate the chat completion processor.
func ChatCompletionProcessorFactory(ccm x.ChatCompletionMetrics) ProcessorFactory {
	return func(config *processorConfig, requestHeaders map[string]string, logger *slog.Logger, isUpstreamFilter bool) (Processor, error) {
		if config.schema.Name != filterapi.APISchemaOpenAI {
			return nil, fmt.Errorf("unsupported API schema: %s", config.schema.Name)
		}
		logger = logger.With("processor", "chat-completion", "isUpstreamFilter", fmt.Sprintf("%v", isUpstreamFilter))
		if !isUpstreamFilter {
			return &chatCompletionProcessorRouterFilter{
				config:         config,
				requestHeaders: requestHeaders,
				logger:         logger,
			}, nil
		}
		return &chatCompletionProcessorUpstreamFilter{
			config:         config,
			requestHeaders: requestHeaders,
			logger:         logger,
			metrics:        ccm,
		}, nil
	}
}

// chatCompletionProcessorRouterFilter implements [Processor] for the `/v1/chat/completion` endpoint.
//
// This is primarily used to select the route for the request based on the model name.
type chatCompletionProcessorRouterFilter struct {
	passThroughProcessor
	// upstreamFilter is the upstream filter that is used to process the request at the upstream filter.
	// This will be updated when the request is retried.
	//
	// On the response handling path, we don't need to do any operation until successful, so we use the implementation
	// of the upstream filter to handle the response at the router filter.
	//
	// TODO: this is a bit of a hack and dirty workaround, so revert this to a cleaner design later.
	upstreamFilter Processor
	logger         *slog.Logger
	config         *processorConfig
	requestHeaders map[string]string
	// originalRequestBody is the original request body that is passed to the upstream filter.
	// This is used to perform the transformation of the request body on the original input
	// when the request is retried.
	originalRequestBody    *openai.ChatCompletionRequest
	originalRequestBodyRaw []byte
	// upstreamFilterCount is the number of upstream filters that have been processed.
	// This is used to determine if the request is a retry request.
	upstreamFilterCount int
}

// ProcessResponseHeaders implements [Processor.ProcessResponseHeaders].
func (c *chatCompletionProcessorRouterFilter) ProcessResponseHeaders(ctx context.Context, headerMap *corev3.HeaderMap) (*extprocv3.ProcessingResponse, error) {
	// If the request failed to route and/or immediate response was returned before the upstream filter was set,
	// c.upstreamFilter can be nil.
	if c.upstreamFilter != nil { // See the comment on the "upstreamFilter" field.
		return c.upstreamFilter.ProcessResponseHeaders(ctx, headerMap)
	}
	return c.passThroughProcessor.ProcessResponseHeaders(ctx, headerMap)
}

// ProcessResponseBody implements [Processor.ProcessResponseBody].
func (c *chatCompletionProcessorRouterFilter) ProcessResponseBody(ctx context.Context, body *extprocv3.HttpBody) (*extprocv3.ProcessingResponse, error) {
	// If the request failed to route and/or immediate response was returned before the upstream filter was set,
	// c.upstreamFilter can be nil.
	if c.upstreamFilter != nil { // See the comment on the "upstreamFilter" field.
		return c.upstreamFilter.ProcessResponseBody(ctx, body)
	}
	return c.passThroughProcessor.ProcessResponseBody(ctx, body)
}

// ProcessRequestBody implements [Processor.ProcessRequestBody].
func (c *chatCompletionProcessorRouterFilter) ProcessRequestBody(_ context.Context, rawBody *extprocv3.HttpBody) (*extprocv3.ProcessingResponse, error) {
	model, body, err := parseOpenAIChatCompletionBody(rawBody)
	if err != nil {
		return nil, fmt.Errorf("failed to parse request body: %w", err)
	}

	c.requestHeaders[c.config.modelNameHeaderKey] = model

	var additionalHeaders []*corev3.HeaderValueOption
	additionalHeaders = append(additionalHeaders, &corev3.HeaderValueOption{
		// Set the model name to the request header with the key `x-ai-eg-model`.
		Header: &corev3.HeaderValue{Key: c.config.modelNameHeaderKey, RawValue: []byte(model)},
	}, &corev3.HeaderValueOption{
		Header: &corev3.HeaderValue{Key: originalPathHeader, RawValue: []byte(c.requestHeaders[":path"])},
	})
	c.originalRequestBody = body
	c.originalRequestBodyRaw = rawBody.Body
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestBody{
			RequestBody: &extprocv3.BodyResponse{
				Response: &extprocv3.CommonResponse{
					HeaderMutation: &extprocv3.HeaderMutation{
						SetHeaders: additionalHeaders,
					},
					ClearRouteCache: true,
				},
			},
		},
	}, nil
}

// chatCompletionProcessorUpstreamFilter implements [Processor] for the `/v1/chat/completion` endpoint at the upstream filter.
//
// This is created per retry and handles the translation as well as the authentication of the request.
type chatCompletionProcessorUpstreamFilter struct {
	logger                 *slog.Logger
	config                 *processorConfig
	requestHeaders         map[string]string
	responseHeaders        map[string]string
	responseEncoding       string
	modelNameOverride      string
	backendName            string
	handler                backendauth.Handler
	originalRequestBodyRaw []byte
	originalRequestBody    *openai.ChatCompletionRequest
	translator             translator.OpenAIChatCompletionTranslator
	// onRetry is true if this is a retry request at the upstream filter.
	onRetry bool
	// cost is the cost of the request that is accumulated during the processing of the response.
	costs translator.LLMTokenUsage
	// metrics tracking.
	metrics x.ChatCompletionMetrics
	// stream is set to true if the request is a streaming request.
	stream bool
}

// selectTranslator selects the translator based on the output schema.
func (c *chatCompletionProcessorUpstreamFilter) selectTranslator(out filterapi.VersionedAPISchema) error {
	switch out.Name {
	case filterapi.APISchemaOpenAI:
		c.translator = translator.NewChatCompletionOpenAIToOpenAITranslator(out.Version, c.modelNameOverride)
	case filterapi.APISchemaAWSBedrock:
		c.translator = translator.NewChatCompletionOpenAIToAWSBedrockTranslator(c.modelNameOverride)
	case filterapi.APISchemaAzureOpenAI:
		c.translator = translator.NewChatCompletionOpenAIToAzureOpenAITranslator(out.Version, c.modelNameOverride)
	case filterapi.APISchemaGCPVertexAI:
		c.translator = translator.NewChatCompletionOpenAIToGCPVertexAITranslator(c.modelNameOverride)
	case filterapi.APISchemaGCPAnthropic:
		c.translator = translator.NewChatCompletionOpenAIToGCPAnthropicTranslator(out.Version, c.modelNameOverride)
	default:
		return fmt.Errorf("unsupported API schema: backend=%s", out)
	}
	return nil
}

// ProcessRequestHeaders implements [Processor.ProcessRequestHeaders].
//
// At the upstream filter, we already have the original request body at request headers phase.
// So, we simply do the translation and upstream auth at this stage, and send them back to Envoy
// with the status CONTINUE_AND_REPLACE. This will allows Envoy to not send the request body again
// to the extproc.
func (c *chatCompletionProcessorUpstreamFilter) ProcessRequestHeaders(ctx context.Context, _ *corev3.HeaderMap) (res *extprocv3.ProcessingResponse, err error) {
	defer func() {
		if err != nil {
			c.metrics.RecordRequestCompletion(ctx, false)
		}
	}()

	// Start tracking metrics for this request.
	c.metrics.StartRequest(c.requestHeaders)
	c.metrics.SetModel(c.requestHeaders[c.config.modelNameHeaderKey])

	headerMutation, bodyMutation, err := c.translator.RequestBody(c.originalRequestBodyRaw, c.originalRequestBody, c.onRetry)
	if err != nil {
		return nil, fmt.Errorf("failed to transform request: %w", err)
	}
	if headerMutation == nil {
		headerMutation = &extprocv3.HeaderMutation{}
	} else {
		for _, h := range headerMutation.SetHeaders {
			c.requestHeaders[h.Header.Key] = string(h.Header.RawValue)
		}
	}
	if h := c.handler; h != nil {
		if err = h.Do(ctx, c.requestHeaders, headerMutation, bodyMutation); err != nil {
			return nil, fmt.Errorf("failed to do auth request: %w", err)
		}
	}

	var dm *structpb.Struct
	if bm := bodyMutation.GetBody(); bm != nil {
		dm = buildContentLengthDynamicMetadataOnRequest(c.config, len(bm))
	}

	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{
				Response: &extprocv3.CommonResponse{
					HeaderMutation: headerMutation, BodyMutation: bodyMutation,
					Status: extprocv3.CommonResponse_CONTINUE_AND_REPLACE,
				},
			},
		},
		DynamicMetadata: dm,
	}, nil
}

// ProcessRequestBody implements [Processor.ProcessRequestBody].
func (c *chatCompletionProcessorUpstreamFilter) ProcessRequestBody(context.Context, *extprocv3.HttpBody) (res *extprocv3.ProcessingResponse, err error) {
	panic("BUG: ProcessRequestBody should not be called in the upstream filter")
}

// ProcessResponseHeaders implements [Processor.ProcessResponseHeaders].
func (c *chatCompletionProcessorUpstreamFilter) ProcessResponseHeaders(ctx context.Context, headers *corev3.HeaderMap) (res *extprocv3.ProcessingResponse, err error) {
	defer func() {
		if err != nil {
			c.metrics.RecordRequestCompletion(ctx, false)
		}
	}()

	c.responseHeaders = headersToMap(headers)
	if enc := c.responseHeaders["content-encoding"]; enc != "" {
		c.responseEncoding = enc
	}
	headerMutation, err := c.translator.ResponseHeaders(c.responseHeaders)
	if err != nil {
		return nil, fmt.Errorf("failed to transform response headers: %w", err)
	}
	var mode *extprocv3http.ProcessingMode
	if c.stream && c.responseHeaders[":status"] == "200" {
		// We only stream the response if the status code is 200 and the response is a stream.
		mode = &extprocv3http.ProcessingMode{ResponseBodyMode: extprocv3http.ProcessingMode_STREAMED}
	}
	return &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_ResponseHeaders{
		ResponseHeaders: &extprocv3.HeadersResponse{
			Response: &extprocv3.CommonResponse{HeaderMutation: headerMutation},
		},
	}, ModeOverride: mode}, nil
}

// ProcessResponseBody implements [Processor.ProcessResponseBody].
func (c *chatCompletionProcessorUpstreamFilter) ProcessResponseBody(ctx context.Context, body *extprocv3.HttpBody) (res *extprocv3.ProcessingResponse, err error) {
	defer func() {
		c.metrics.RecordRequestCompletion(ctx, err == nil)
	}()
	var br io.Reader
	var isGzip bool
	switch c.responseEncoding {
	case "gzip":
		br, err = gzip.NewReader(bytes.NewReader(body.Body))
		if err != nil {
			return nil, fmt.Errorf("failed to decode gzip: %w", err)
		}
		isGzip = true
	default:
		br = bytes.NewReader(body.Body)
	}

	headerMutation, bodyMutation, tokenUsage, err := c.translator.ResponseBody(c.responseHeaders, br, body.EndOfStream)
	if err != nil {
		return nil, fmt.Errorf("failed to transform response: %w", err)
	}
	if bodyMutation != nil && isGzip {
		if headerMutation == nil {
			headerMutation = &extprocv3.HeaderMutation{}
		}
		// TODO: this is a hotfix, we should update this to recompress since its in the header
		// If the response was gzipped, ensure we remove the content-encoding header.
		//
		// This is only needed when the transformation is actually modifying the body. When the backend
		// is in OpenAI format (and it's the first try before any retry), the response body is not modified,
		// so we don't need to remove the header in that case.
		headerMutation.RemoveHeaders = append(headerMutation.RemoveHeaders, "content-encoding")
	}

	resp := &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ResponseBody{
			ResponseBody: &extprocv3.BodyResponse{
				Response: &extprocv3.CommonResponse{
					HeaderMutation: headerMutation,
					BodyMutation:   bodyMutation,
				},
			},
		},
	}

	// TODO: we need to investigate if we need to accumulate the token usage for streaming responses.
	c.costs.InputTokens += tokenUsage.InputTokens
	c.costs.OutputTokens += tokenUsage.OutputTokens
	c.costs.TotalTokens += tokenUsage.TotalTokens

	// Update metrics with token usage.
	c.metrics.RecordTokenUsage(ctx, tokenUsage.InputTokens, tokenUsage.OutputTokens, tokenUsage.TotalTokens)
	if c.stream {
		// Token latency is only recorded for streaming responses, otherwise it doesn't make sense since
		// these metrics are defined as a difference between the two output events.
		c.metrics.RecordTokenLatency(ctx, tokenUsage.OutputTokens)
	}

	if body.EndOfStream && len(c.config.requestCosts) > 0 {
		metadata, err := buildDynamicMetadata(c.config, &c.costs, c.requestHeaders, c.modelNameOverride, c.backendName)
		if err != nil {
			return nil, fmt.Errorf("failed to build dynamic metadata: %w", err)
		}
		if c.stream {
			// Adding token latency information to metadata.
			c.mergeWithTokenLatencyMetadata(metadata)
		}
		resp.DynamicMetadata = metadata
	}

	return resp, nil
}

// SetBackend implements [Processor.SetBackend].
func (c *chatCompletionProcessorUpstreamFilter) SetBackend(ctx context.Context, b *filterapi.Backend, backendHandler backendauth.Handler, routeProcessor Processor) (err error) {
	defer func() {
		c.metrics.RecordRequestCompletion(ctx, err == nil)
	}()
	rp, ok := routeProcessor.(*chatCompletionProcessorRouterFilter)
	if !ok {
		panic("BUG: expected routeProcessor to be of type *chatCompletionProcessorRouterFilter")
	}
	rp.upstreamFilterCount++
	c.metrics.SetBackend(b)
	c.modelNameOverride = b.ModelNameOverride
	c.backendName = b.Name
	if err = c.selectTranslator(b.Schema); err != nil {
		return fmt.Errorf("failed to select translator: %w", err)
	}
	c.handler = backendHandler
	c.originalRequestBody = rp.originalRequestBody
	c.originalRequestBodyRaw = rp.originalRequestBodyRaw
	c.onRetry = rp.upstreamFilterCount > 1
	c.stream = c.originalRequestBody.Stream
	rp.upstreamFilter = c
	return
}

func (c *chatCompletionProcessorUpstreamFilter) mergeWithTokenLatencyMetadata(metadata *structpb.Struct) {
	timeToFirstTokenMs := c.metrics.GetTimeToFirstTokenMs()
	interTokenLatencyMs := c.metrics.GetInterTokenLatencyMs()
	ns := c.config.metadataNamespace
	innerVal := metadata.Fields[ns].GetStructValue()
	if innerVal == nil {
		innerVal = &structpb.Struct{Fields: map[string]*structpb.Value{}}
		metadata.Fields[ns] = structpb.NewStructValue(innerVal)
	}
	innerVal.Fields["token_latency_ttft"] = &structpb.Value{Kind: &structpb.Value_NumberValue{NumberValue: timeToFirstTokenMs}}
	innerVal.Fields["token_latency_itl"] = &structpb.Value{Kind: &structpb.Value_NumberValue{NumberValue: interTokenLatencyMs}}
}

func parseOpenAIChatCompletionBody(body *extprocv3.HttpBody) (modelName string, rb *openai.ChatCompletionRequest, err error) {
	var openAIReq openai.ChatCompletionRequest
	if err := json.Unmarshal(body.Body, &openAIReq); err != nil {
		return "", nil, fmt.Errorf("failed to unmarshal body: %w", err)
	}
	return openAIReq.Model, &openAIReq, nil
}

// buildContentLengthDynamicMetadataOnRequest builds dynamic metadata for the request with content length.
//
// This is necessary to ensure that the content length can be set after the extproc filter has processed the request,
// which will happen in the header mutation filter.
//
// This is needed since the content length header is unconditionally cleared by Envoy as we use REPLACE_AND_CONTINUE
// processing mode in the request headers phase at upstream filter. This is sort of a workaround, and it is necessary
// for now.
func buildContentLengthDynamicMetadataOnRequest(config *processorConfig, contentLength int) *structpb.Struct {
	metadata := &structpb.Struct{
		Fields: map[string]*structpb.Value{
			config.metadataNamespace: {
				Kind: &structpb.Value_StructValue{
					StructValue: &structpb.Struct{
						Fields: map[string]*structpb.Value{
							"content_length": {
								Kind: &structpb.Value_NumberValue{NumberValue: float64(contentLength)},
							},
						},
					},
				},
			},
		},
	}
	return metadata
}

func buildDynamicMetadata(config *processorConfig, costs *translator.LLMTokenUsage, requestHeaders map[string]string, modelNameOverride, backendName string) (*structpb.Struct, error) {
	metadataCost := make(map[string]*structpb.Value, len(config.requestCosts))
	for i := range config.requestCosts {
		rc := &config.requestCosts[i]
		var cost uint32
		switch rc.Type {
		case filterapi.LLMRequestCostTypeInputToken:
			cost = costs.InputTokens
		case filterapi.LLMRequestCostTypeOutputToken:
			cost = costs.OutputTokens
		case filterapi.LLMRequestCostTypeTotalToken:
			cost = costs.TotalTokens
		case filterapi.LLMRequestCostTypeCEL:
			costU64, err := llmcostcel.EvaluateProgram(
				rc.celProg,
				requestHeaders[config.modelNameHeaderKey],
				backendName,
				costs.InputTokens,
				costs.OutputTokens,
				costs.TotalTokens,
			)
			if err != nil {
				return nil, fmt.Errorf("failed to evaluate CEL expression: %w", err)
			}
			cost = uint32(costU64) //nolint:gosec
		default:
			return nil, fmt.Errorf("unknown request cost kind: %s", rc.Type)
		}
		metadataCost[rc.MetadataKey] = &structpb.Value{Kind: &structpb.Value_NumberValue{NumberValue: float64(cost)}}
	}

	metadataRoute := make(map[string]*structpb.Value, 2)
	if modelNameOverride != "" {
		metadataRoute["model_name_override"] = &structpb.Value{Kind: &structpb.Value_StringValue{StringValue: modelNameOverride}}
	}

	if backendName != "" {
		metadataRoute["backend_name"] = &structpb.Value{Kind: &structpb.Value_StringValue{StringValue: backendName}}
	}

	metadata := &structpb.Struct{
		Fields: map[string]*structpb.Value{},
	}

	if len(metadataCost) != 0 {
		metadata.Fields[config.metadataNamespace] = &structpb.Value{Kind: &structpb.Value_StructValue{StructValue: &structpb.Struct{Fields: metadataCost}}}
	}

	if len(metadataRoute) != 0 {
		metadata.Fields["route"] = &structpb.Value{Kind: &structpb.Value_StructValue{StructValue: &structpb.Struct{Fields: metadataRoute}}}
	}

	if len(metadata.Fields) == 0 {
		return nil, nil
	}

	return metadata, nil
}
