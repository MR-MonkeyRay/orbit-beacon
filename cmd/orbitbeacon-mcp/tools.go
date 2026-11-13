package main

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"orbitbeacon/internal/uuid"
)

type property struct {
	Type      string `json:"type"`
	Minimum   *int   `json:"minimum,omitempty"`
	Maximum   *int   `json:"maximum,omitempty"`
	Enum      any    `json:"enum,omitempty"`
	Format    string `json:"format,omitempty"`
	MinLength int    `json:"minLength,omitempty"`
	MaxLength int    `json:"maxLength,omitempty"`
}
type inputSchema struct {
	Type                 string              `json:"type"`
	Properties           map[string]property `json:"properties"`
	Required             []string            `json:"required,omitempty"`
	AdditionalProperties bool                `json:"additionalProperties"`
}
type annotations struct {
	ReadOnlyHint    bool `json:"readOnlyHint"`
	DestructiveHint bool `json:"destructiveHint"`
	IdempotentHint  bool `json:"idempotentHint"`
	OpenWorldHint   bool `json:"openWorldHint"`
}
type tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema inputSchema `json:"inputSchema"`
	Annotations annotations `json:"annotations"`
	path        string
}

func integer(minimum int, maximum *int, enum ...int) property {
	p := property{Type: "integer", Minimum: &minimum, Maximum: maximum}
	if len(enum) != 0 {
		p.Enum = enum
	}
	return p
}
func text(enum ...string) property {
	p := property{Type: "string", MinLength: 1}
	if len(enum) != 0 {
		p.Enum = enum
	}
	return p
}
func toolTable() []tool {
	hundred, fourHundred := 100, 400
	id := property{Type: "string", Format: "uuid"}
	limit := integer(1, &hundred)
	makeTool := func(name, description, path string, props map[string]property, required ...string) tool {
		return tool{name, description, inputSchema{"object", props, required, false}, annotations{true, false, true, false}, path}
	}
	return []tool{
		makeTool("list_services", "List server-monitored services and latest results.", "/api/v1/services", map[string]property{}),
		makeTool("get_service", "Get a server-monitored service and its latest result.", "/api/v1/services/{id}", map[string]property{"id": id}, "id"),
		makeTool("get_service_history", "Get availability and latency history for a service.", "/api/v1/services/{id}/history", map[string]property{"id": id, "range": text("1h", "24h", "7d", "30d")}, "id", "range"),
		makeTool("list_nodes", "List monitored nodes.", "/api/v1/nodes", map[string]property{"limit": limit, "offset": integer(0, nil)}),
		makeTool("get_node", "Get a monitored node.", "/api/v1/nodes/{id}", map[string]property{"id": id}, "id"),
		makeTool("get_node_latest", "Get the latest metrics for a node.", "/api/v1/nodes/{id}/latest", map[string]property{"id": id}, "id"),
		makeTool("get_node_history", "Get metric history for a node.", "/api/v1/nodes/{id}/history", map[string]property{
			"id": id, "metric": text("cpu_percent", "memory_used_percent", "swap_used_percent", "load1", "load5", "load15", "disk_used_percent", "inode_used_percent", "net_rx_bytes_per_second", "net_tx_bytes_per_second", "disk_read_bytes_per_second", "disk_write_bytes_per_second", "disk_busy_percent", "pressure_cpu_percent", "pressure_memory_percent", "pressure_io_percent", "temperature_celsius"),
			"range": text("1h", "6h", "24h", "7d", "30d", "90d"), "disk": {Type: "string", MinLength: 1, MaxLength: 1024}}, "id", "metric", "range"),
		makeTool("get_node_availability", "Get availability for a node.", "/api/v1/nodes/{id}/availability", map[string]property{"id": id, "days": integer(30, nil, 30, 90)}, "id"),
		makeTool("get_node_traffic", "Get daily traffic for a node.", "/api/v1/nodes/{id}/traffic", map[string]property{"id": id, "days": integer(1, &fourHundred)}, "id"),
		makeTool("get_node_probes", "Get service probes for a node.", "/api/v1/nodes/{id}/probes", map[string]property{"id": id}, "id"),
		makeTool("list_alerts", "List alert events.", "/api/v1/alerts", map[string]property{"state": text("firing", "resolved", "all"), "limit": limit, "before": {Type: "string", MinLength: 1, MaxLength: 256}}),
		makeTool("get_alert_summary", "Get the alert event summary.", "/api/v1/alerts/summary", map[string]property{}),
		makeTool("get_alert", "Get an alert event.", "/api/v1/alerts/{id}", map[string]property{"id": id}, "id"),
		makeTool("list_groups", "List node groups ordered by display_order descending, then name and id.", "/api/v1/groups", map[string]property{}),
	}
}

var jsonInteger = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

func (t tool) requestTarget(arguments json.RawMessage) (string, bool) {
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(arguments, &args) != nil || args == nil {
		return "", false
	}
	for _, key := range t.InputSchema.Required {
		if _, ok := args[key]; !ok {
			return "", false
		}
	}
	query := url.Values{}
	path := t.path
	for key, raw := range args {
		p, ok := t.InputSchema.Properties[key]
		if !ok {
			return "", false
		}
		var value string
		if p.Type == "integer" {
			if !jsonInteger.Match(bytes.TrimSpace(raw)) {
				return "", false
			}
			n, err := strconv.Atoi(string(bytes.TrimSpace(raw)))
			if err != nil || p.Minimum != nil && n < *p.Minimum || p.Maximum != nil && n > *p.Maximum {
				return "", false
			}
			if choices, ok := p.Enum.([]int); ok {
				found := false
				for _, choice := range choices {
					found = found || n == choice
				}
				if !found {
					return "", false
				}
			}
			value = strconv.Itoa(n)
		} else {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil || len(value) < p.MinLength || p.MaxLength > 0 && len(value) > p.MaxLength {
				return "", false
			}
			if choices, ok := p.Enum.([]string); ok {
				found := false
				for _, choice := range choices {
					found = found || value == choice
				}
				if !found {
					return "", false
				}
			}
		}
		if key == "id" {
			if !uuid.ValidUUID(value) {
				return "", false
			}
			// Only UUIDs can enter paths; all other values go through url.Values.
			path = strings.Replace(path, "{id}", value, 1)
		} else {
			query.Set(key, value)
		}
	}
	if len(query) != 0 {
		path += "?" + query.Encode()
	}
	return path, true
}
