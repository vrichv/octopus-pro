package relay

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

//go:embed opencode_zen_free_profile.json
var openCodeZenFreeProfile []byte

type openCodeZenFreeOutbound struct {
	transformer.Outbound
	noAuth          bool
	mappings        map[string]*zenToolMapping // upstream name -> client schema mapping
	protectedFields map[string]json.RawMessage
}

func (t *openCodeZenFreeOutbound) TransformRequest(ctx context.Context, request *llm.Request) (*httpclient.Request, error) {
	if request == nil {
		return nil, fmt.Errorf("OpenCode Zen Free: missing request")
	}
	var profile struct {
		System []llm.Message `json:"system"`
		Tools  []llm.Tool    `json:"tools"`
	}
	if err := json.Unmarshal(openCodeZenFreeProfile, &profile); err != nil {
		return nil, fmt.Errorf("OpenCode Zen Free profile: %w", err)
	}

	// OpenCode's ProviderTransform.schema() emits model-specific tool schemas
	// on the OpenAI-compatible Chat path. Keep the client's own declarations
	// there instead of overwriting them with the captured Responses profile.
	// The free tier still requires `bash` and `read` in tools (see
	// opencode-zen-free-tier.md); add the profile definitions only when the
	// client did not declare them. Responses keeps the pinned profile plus
	// the namespace/schema restoration below.
	adapted := *request
	adapted.TransformerMetadata = maps.Clone(request.TransformerMetadata)
	adapted.ProviderExtensions = llm.CloneProviderExtensions(request.ProviderExtensions)
	adapted.Messages = slices.Clone(profile.System)
	adapted.Tools = slices.Clone(profile.Tools)
	adapted.Stream = new(true)
	adapted.TransformOptions.ArrayInputs = new(true)
	chatTools := request.APIFormat == llm.APIFormatOpenAIChatCompletion && len(request.Tools) > 0
	if chatTools {
		adapted.Tools = slices.Clone(request.Tools)
		for _, template := range profile.Tools {
			if template.Function.Name != "bash" && template.Function.Name != "read" {
				continue
			}
			if !slices.ContainsFunc(request.Tools, func(tool llm.Tool) bool {
				return tool.Type == "function" && tool.Function.Name == template.Function.Name
			}) {
				adapted.Tools = append(adapted.Tools, template)
			}
		}
	}
	if request.ToolChoice == nil {
		adapted.ToolChoice = &llm.ToolChoice{ToolChoice: new("auto")}
	} else {
		choice := *request.ToolChoice
		adapted.ToolChoice = &choice
		if choice.NamedToolChoice != nil {
			named := *choice.NamedToolChoice
			adapted.ToolChoice.NamedToolChoice = &named
		}
	}

	var mappings map[string]*zenToolMapping
	var extraTools []llm.Tool
	var err error
	if !chatTools {
		mappings, extraTools, err = zenToolMappings(profile.Tools, request.Tools)
		if err != nil {
			return nil, err
		}
	}
	t.mappings = mappings
	adapted.Tools = append(adapted.Tools, extraTools...)
	byClient := make(map[string]*zenToolMapping, len(mappings))
	for _, mapping := range mappings {
		if !mapping.keepClient {
			byClient[mapping.clientName] = mapping
		}
	}
	for _, original := range request.Messages {
		if original.Role == "system" || original.Role == "developer" {
			continue
		}
		message := original
		message.ToolCalls = slices.Clone(original.ToolCalls)
		for i := range message.ToolCalls {
			call := &message.ToolCalls[i]
			if mapping := byClient[call.Function.Name]; mapping != nil && call.Type != "custom" {
				call.Function.Arguments, err = mapping.arguments(call.Function.Arguments, true)
				if err != nil {
					return nil, err
				}
				call.Function.Name = mapping.upstreamName
			}
		}
		if message.ToolCallName != nil {
			if mapping := byClient[*message.ToolCallName]; mapping != nil {
				message.ToolCallName = new(mapping.upstreamName)
			}
		}
		adapted.Messages = append(adapted.Messages, message)
	}
	if named := adapted.ToolChoice.NamedToolChoice; named != nil {
		if mapping := byClient[named.Function.Name]; mapping != nil {
			named.Function.Name = mapping.upstreamName
		}
	}

	// Responses retains namespace declarations and allowed_tools choices in
	// provider sidecars. Preserve namespaces and map any consumed tool aliases
	// in allowed_tools just as we map the structured named choice.
	if ext := adapted.ProviderExtensions; ext != nil && ext.OpenAIResponses != nil && ext.OpenAIResponses.Request != nil {
		raw := ext.OpenAIResponses.Request
		for _, fragment := range raw.RawTools {
			if fragment.Type == "namespace" {
				for _, mapping := range mappings {
					if strings.HasPrefix(mapping.clientName, fragment.Name+"__") || strings.HasPrefix(mapping.clientName, fragment.Name+".") {
						mapping.clientNamespace = fragment.Name
					}
				}
			}
		}
		if len(raw.RawToolChoice) > 0 {
			raw.RawToolChoice, err = zenMapRawToolChoice(raw.RawToolChoice, byClient)
			if err != nil {
				return nil, err
			}
		}
	}
	raw, err := t.Outbound.TransformRequest(ctx, &adapted)
	if err != nil {
		return nil, err
	}
	if t.noAuth {
		raw.Auth = nil
		raw.Headers.Del("Authorization")
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw.Body, &body); err != nil {
		return nil, err
	}
	t.protectedFields = make(map[string]json.RawMessage)
	for _, field := range []string{"stream", "messages", "system", "input", "instructions", "tools", "tool_choice", "contents", "systemInstruction", "toolConfig"} {
		if value, ok := body[field]; ok {
			t.protectedFields[field] = slices.Clone(value)
		}
	}
	return raw, nil
}

func zenMapRawToolChoice(raw json.RawMessage, mappings map[string]*zenToolMapping) (json.RawMessage, error) {
	var choice map[string]json.RawMessage
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil, err
	}
	var name string
	if json.Unmarshal(choice["name"], &name) == nil {
		if mapping := mappings[name]; mapping != nil {
			choice["name"], _ = json.Marshal(mapping.upstreamName)
		}
	}
	if nested, ok := choice["function"]; ok {
		mapped, err := zenMapRawToolChoice(nested, mappings)
		if err != nil {
			return nil, err
		}
		choice["function"] = mapped
	}
	if nested, ok := choice["tools"]; ok {
		var tools []json.RawMessage
		if err := json.Unmarshal(nested, &tools); err != nil {
			return nil, err
		}
		for i := range tools {
			mapped, err := zenMapRawToolChoice(tools[i], mappings)
			if err != nil {
				return nil, err
			}
			tools[i] = mapped
		}
		choice["tools"], _ = json.Marshal(tools)
	}
	return json.Marshal(choice)
}

func (t *openCodeZenFreeOutbound) TransformStream(ctx context.Context, request *httpclient.Request, source streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*llm.Response], error) {
	stream, err := t.Outbound.TransformStream(ctx, request, source)
	if err != nil {
		return nil, err
	}
	if len(t.mappings) == 0 {
		return stream, nil
	}
	return &zenToolStream{source: stream, mappings: t.mappings, pending: make(map[zenCallIndex]*zenPendingCall)}, nil
}

// Restore raw Responses namespace/built-in declarations after profile injection
// changes AxonHub's tool signature list. Also retain the original function
// schema instead of applying Responses strict-schema normalization to it.
func zenRestoreResponsesTools(raw *httpclient.Request, request *llm.Request) error {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw.Body, &body); err != nil {
		return err
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(body["tools"], &tools); err != nil {
		return err
	}
	functions := make(map[string]llm.Function)
	for _, tool := range request.Tools {
		if tool.Type == "function" {
			functions[tool.Function.Name] = tool.Function
		}
	}
	for i, tool := range tools {
		var identity struct{ Type, Name string }
		if err := json.Unmarshal(tool, &identity); err != nil {
			return err
		}
		if function, ok := functions[identity.Name]; identity.Type == "function" && ok {
			parameters := function.Parameters
			if len(parameters) == 0 {
				parameters = function.ParametersJsonSchema
			}
			converted := struct {
				Type        string          `json:"type"`
				Name        string          `json:"name"`
				Description string          `json:"description,omitempty"`
				Parameters  json.RawMessage `json:"parameters,omitempty"`
				Strict      *bool           `json:"strict,omitempty"`
			}{"function", function.Name, function.Description, parameters, function.Strict}
			encoded, err := json.Marshal(converted)
			if err != nil {
				return err
			}
			tools[i] = encoded
		}
	}
	if ext := request.ProviderExtensions; ext != nil && ext.OpenAIResponses != nil && ext.OpenAIResponses.Request != nil {
		for _, fragment := range ext.OpenAIResponses.Request.RawTools {
			var definition struct {
				Type, Name string
				Tools      []struct{ Type, Name string }
			}
			if err := json.Unmarshal(fragment.Raw, &definition); err != nil {
				return err
			}
			replaced := make(map[string]bool)
			for _, tool := range definition.Tools {
				if tool.Type == "function" {
					replaced[definition.Name+"__"+tool.Name] = true
					replaced[definition.Name+"."+tool.Name] = true
				}
			}
			tools = slices.DeleteFunc(tools, func(tool json.RawMessage) bool {
				var identity struct{ Type, Name string }
				_ = json.Unmarshal(tool, &identity)
				return (identity.Type == definition.Type && identity.Name == definition.Name) || (identity.Type == "function" && replaced[identity.Name])
			})
			tools = append(tools, slices.Clone(fragment.Raw))
		}
		if choice := ext.OpenAIResponses.Request.RawToolChoice; len(choice) > 0 {
			body["tool_choice"] = slices.Clone(choice)
		}
	}
	encoded, err := json.Marshal(tools)
	if err != nil {
		return err
	}
	body["tools"] = encoded
	raw.Body, err = json.Marshal(body)
	return err
}

// Channel overrides may still change model tuning, but cannot undo the Free
// profile or the mapped history/tool choice after TransformRequest has run.
func (t *openCodeZenFreeOutbound) enforceProfile(raw *httpclient.Request) error {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw.Body, &body); err != nil {
		return err
	}
	for field, value := range t.protectedFields {
		body[field] = value
	}
	var err error
	raw.Body, err = json.Marshal(body)
	return err
}
