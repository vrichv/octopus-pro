package relay

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/looplj/axonhub/llm"
)

type zenToolSchema struct {
	Type       string                     `json:"type"`
	Properties map[string]json.RawMessage `json:"properties"`
	Required   []string                   `json:"required"`
}

type zenToolMapping struct {
	upstreamName    string
	clientName      string
	clientNamespace string
	// Keep native aliases when they expose controls absent from the profile,
	// such as exec_command's yielding and output limits. Their histories and
	// explicit tool choices continue to use their native upstream declaration.
	keepClient     bool
	fields         map[string]string // profile field -> client field
	upstream       zenToolSchema
	client         zenToolSchema
	timeoutSeconds bool
	description    bool
	clientFill     map[string]json.RawMessage
}

var zenToolAliases = map[string][]string{
	"bash":  {"Bash", "exec_command", "shell_command"},
	"read":  {"Read", "read_file"},
	"edit":  {"Edit"},
	"write": {"Write", "write_file"},
	"glob":  {"Glob"},
	"grep":  {"Grep"},
}

var zenFieldAliases = map[string][]string{
	"filePath":   {"path", "file_path"},
	"oldString":  {"oldText", "old_text", "old_string"},
	"newString":  {"newText", "new_text", "new_string"},
	"replaceAll": {"replace_all"},
	"command":    {"cmd"},
	"workdir":    {"cwd"},
	"timeout":    {"timeout_ms"},
	"offset":     {"start_line"},
	"limit":      {"max_lines"},
}

func zenToolMappings(profile, client []llm.Tool) (map[string]*zenToolMapping, []llm.Tool, error) {
	mappings := make(map[string]*zenToolMapping)
	used := make(map[string]bool)
	profileNames := make(map[string]bool)
	for _, template := range profile {
		name := template.Function.Name
		profileNames[name] = true
		var selected *llm.Tool
		for i := range client {
			if client[i].Function.Name != name {
				continue
			}
			if candidate, err := zenBuildToolMapping(template, client[i]); err == nil {
				mappings[name] = candidate
				selected = &client[i]
			}
			break
		}
		if selected == nil {
			for _, alias := range zenToolAliases[name] {
				for i := range client {
					if client[i].Type == "function" && (client[i].Function.Name == alias || strings.HasSuffix(client[i].Function.Name, "."+alias) || strings.HasSuffix(client[i].Function.Name, "__"+alias)) {
						candidate, err := zenBuildToolMapping(template, client[i])
						if err == nil {
							mappings[name] = candidate
							selected = &client[i]
							break
						}
					}
				}
				if selected != nil {
					break
				}
			}
		}
		if selected == nil {
			continue
		}
		mapping := mappings[name]
		if !mapping.keepClient {
			used[selected.Function.Name] = true
		}
	}
	var extras []llm.Tool
	for _, tool := range client {
		if tool.Type == "function" {
			if used[tool.Function.Name] || profileNames[tool.Function.Name] {
				continue
			}
			used[tool.Function.Name] = true
		}
		extras = append(extras, tool)
	}
	return mappings, extras, nil
}

func zenBuildToolMapping(template, tool llm.Tool) (*zenToolMapping, error) {
	m := &zenToolMapping{upstreamName: template.Function.Name, clientName: tool.Function.Name, fields: make(map[string]string)}
	fail := func() (*zenToolMapping, error) {
		return nil, fmt.Errorf("OpenCode Zen Free: incompatible schema for client tool %q (profile %q)", m.clientName, m.upstreamName)
	}
	if tool.Type != "function" {
		return fail()
	}
	parameters := tool.Function.Parameters
	if len(parameters) == 0 {
		parameters = tool.Function.ParametersJsonSchema
	}
	if json.Unmarshal(template.Function.Parameters, &m.upstream) != nil || json.Unmarshal(parameters, &m.client) != nil || m.client.Type != "object" {
		return fail()
	}
	for field, schema := range m.upstream.Properties {
		candidates := append([]string{field}, zenFieldAliases[field]...)
		for _, candidate := range candidates {
			if other, exists := m.client.Properties[candidate]; exists && zenSchemaCompatible(schema, other) {
				m.fields[field] = candidate
				break
			}
		}
	}
	for _, required := range m.upstream.Required {
		if m.fields[required] == "" {
			return fail()
		}
	}
	m.clientFill = make(map[string]json.RawMessage)
	for _, required := range m.client.Required {
		var property map[string]json.RawMessage
		_ = json.Unmarshal(m.client.Properties[required], &property)
		if value, ok := property["default"]; ok {
			m.clientFill[required] = value
		} else if zenSchemaNullable(m.client.Properties[required]) {
			m.clientFill[required] = json.RawMessage("null")
		}
		found := false
		for _, target := range m.fields {
			if target == required {
				found = true
			}
		}
		if !found {
			if _, ok := m.clientFill[required]; ok {
				continue
			}
			// A display label has no execution semantics; use the actual command.
			if m.upstreamName == "bash" && required == "description" && zenSchemaType(m.client.Properties[required]) == "string" {
				m.description = true
				continue
			}
			return fail()
		}
	}
	if m.upstreamName == "bash" && m.fields["timeout"] != "" {
		var field struct {
			Description string `json:"description"`
		}
		_ = json.Unmarshal(m.client.Properties[m.fields["timeout"]], &field)
		description := strings.ToLower(field.Description)
		m.timeoutSeconds = strings.Contains(description, "seconds") && !strings.Contains(description, "milliseconds")
	}
	// Preserve aliases natively when they have additional fields or required
	// fields that are optional upstream. Do not discard client control options.
	m.keepClient = m.clientName != m.upstreamName && (len(m.client.Properties) > len(m.fields) || m.description || strings.Contains(m.clientName, ".") || strings.Contains(m.clientName, "__"))
	return m, nil
}

func zenSchemaType(raw json.RawMessage) string {
	var schema struct {
		Type json.RawMessage `json:"type"`
	}
	if json.Unmarshal(raw, &schema) != nil {
		return ""
	}
	var name string
	if json.Unmarshal(schema.Type, &name) == nil {
		return name
	}
	var names []string
	if json.Unmarshal(schema.Type, &names) != nil {
		return ""
	}
	for _, candidate := range names {
		if candidate == "null" {
			continue
		}
		if name != "" {
			return ""
		} // No guessing for multi-type unions.
		name = candidate
	}
	return name
}

func zenSchemaNullable(raw json.RawMessage) bool {
	var schema struct {
		Type []string `json:"type"`
	}
	return json.Unmarshal(raw, &schema) == nil && slices.Contains(schema.Type, "null")
}

// Description/validation constraints can differ without changing the argument
// layout. Nested objects/arrays must have the same structure; we never guess a
// conversion for unrelated task, question, todo or patch schemas.
func zenSchemaCompatible(a, b json.RawMessage) bool {
	var left, right map[string]any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	lt, rt := zenSchemaType(a), zenSchemaType(b)
	if lt != rt && !(lt == "integer" && rt == "number") {
		return false
	}
	if lt == "object" || lt == "array" {
		clean := func(value map[string]any) map[string]any {
			delete(value, "description")
			delete(value, "title")
			delete(value, "$schema")
			return value
		}
		l, _ := json.Marshal(clean(left))
		r, _ := json.Marshal(clean(right))
		return string(l) == string(r)
	}
	return lt != ""
}

func (m *zenToolMapping) arguments(raw string, toUpstream bool) (string, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &values); err != nil || values == nil {
		return "", fmt.Errorf("OpenCode Zen Free: invalid JSON arguments for %q", m.clientName)
	}
	result := make(map[string]json.RawMessage)
	for key, value := range values {
		if toUpstream && strings.TrimSpace(string(value)) == "null" && zenSchemaNullable(m.client.Properties[key]) {
			// Strict client schemas represent omitted optional arguments as null.
			// The fixed profile uses absence instead; required fields are checked below.
			continue
		}
		target := ""
		if toUpstream {
			for upstream, client := range m.fields {
				if client == key {
					target = upstream
					break
				}
			}
			if key == "description" && m.upstreamName == "bash" {
				continue
			}
		} else {
			target = m.fields[key]
		}
		if target == "" {
			return "", fmt.Errorf("OpenCode Zen Free: tool %q cannot map parameter %q", m.clientName, key)
		}
		if m.timeoutSeconds && ((toUpstream && target == "timeout") || (!toUpstream && key == "timeout")) {
			var number float64
			if json.Unmarshal(value, &number) != nil {
				return "", fmt.Errorf("OpenCode Zen Free: invalid timeout for %q", m.clientName)
			}
			if toUpstream {
				number *= 1000
			} else {
				number /= 1000
			}
			if !toUpstream && zenSchemaType(m.client.Properties[target]) == "integer" && number != math.Trunc(number) {
				return "", fmt.Errorf("OpenCode Zen Free: timeout cannot be represented in whole seconds for %q", m.clientName)
			}
			value, _ = json.Marshal(number)
		}
		result[target] = value
	}
	if !toUpstream {
		for field, value := range m.clientFill {
			if _, exists := result[field]; !exists {
				result[field] = value
			}
		}
		if m.description {
			result["description"] = values["command"]
		}
	}
	required := m.client.Required
	if toUpstream {
		required = m.upstream.Required
	}
	for _, field := range required {
		if _, ok := result[field]; !ok {
			return "", fmt.Errorf("OpenCode Zen Free: tool %q is missing required parameter %q", m.clientName, field)
		}
	}
	// Preserve JSON bytes for the exact schema path, including argument ordering.
	identity := !m.timeoutSeconds && !m.description && len(values) == len(result)
	if identity {
		for key := range values {
			if m.fields[key] != key {
				identity = false
				break
			}
		}
	}
	if identity {
		return raw, nil
	}
	encoded, err := json.Marshal(result)
	return string(encoded), err
}

func (m *zenToolMapping) needsConversion() bool {
	if m.clientName != m.upstreamName || m.timeoutSeconds || m.description {
		return true
	}
	if len(m.fields) != len(m.upstream.Properties) {
		return true
	}
	for key, target := range m.fields {
		if key != target {
			return true
		}
	}
	return !slices.Equal(m.upstream.Required, m.client.Required)
}
