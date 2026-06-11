// Copyright 2026 Intel Corporation. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package deployments

// OTelConfig represents the OpenTelemetry Collector configuration for XPU Manager.
type OTelConfig struct {
	Receivers  OTelReceivers          `json:"receivers"`
	Processors OTelProcessors         `json:"processors"`
	Exporters  OTelExporters          `json:"exporters"`
	Extensions map[string]interface{} `json:"extensions,omitempty"`
	Service    OTelService            `json:"service"`
}

// OTelReceivers holds receiver configurations.
type OTelReceivers struct {
	IntelXPU IntelXPUReceiver `json:"intelxpu"`
}

// IntelXPUReceiver is the configuration for the intelxpu receiver.
type IntelXPUReceiver struct {
	CollectionInterval string                 `json:"collection_interval"`
	InitialDelay       string                 `json:"initial_delay"`
	Timeout            int                    `json:"timeout"`
	SamplingInterval   string                 `json:"sampling_interval"`
	Metrics            map[string]interface{} `json:"metrics,omitempty"`
}

// OTelProcessors holds processor configurations.
type OTelProcessors struct {
	IntelXPUStatus IntelXPUStatusProcessor `json:"intelxpustatus"`
}

// IntelXPUStatusProcessor is the configuration for the intelxpustatus processor.
type IntelXPUStatusProcessor struct {
	Rules []StatusRule `json:"rules"`
}

// StatusRule defines a single health evaluation rule.
type StatusRule struct {
	Name               string            `json:"name"`
	SourceMetric       string            `json:"source_metric"`
	ParentMetric       string            `json:"parent_metric"`
	ParentRefAttribute string            `json:"parent_ref_attribute"`
	ParentFilters      []KeyValues       `json:"parent_filters"`
	ComponentFilters   []KeyValues       `json:"component_filters"`
	CopyAttributes     []string          `json:"copy_attributes"`
	AddAttributes      map[string]string `json:"add_attributes"`
	States             []StatusState     `json:"states"`
}

// KeyValues is a key with a list of matching values used for metric filtering.
type KeyValues struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
}

// StatusState defines a named health state, optionally gated by conditions.
type StatusState struct {
	StateName  string           `json:"state_name"`
	Conditions []StateCondition `json:"conditions,omitempty"`
}

// StateCondition is a threshold condition for a health state, with optional device filters.
type StateCondition struct {
	Value         float64     `json:"value"`
	ParentFilters []KeyValues `json:"parent_filters,omitempty"`
}

// OTelExporters holds exporter configurations.
type OTelExporters struct {
	IntelXPUInfo IntelXPUInfoExporter `json:"intelxpuinfo"`
	Prometheus   PrometheusExporter   `json:"prometheus"`
}

// IntelXPUInfoExporter exports health status over a unix domain socket.
type IntelXPUInfoExporter struct {
	Endpoint         string            `json:"endpoint,omitempty"`
	HWStatusMappings []HWStatusMapping `json:"hw_status_mappings,omitempty"`
}

// HWStatusMapping maps a health domain to state severity entries.
type HWStatusMapping struct {
	HealthDomain string                       `json:"health_domain"`
	Filters      []KeyValues                  `json:"filters,omitempty"`
	StateMapping map[string]HWSeverityMapping `json:"state_mapping"`
}

// HWSeverityMapping maps a health state name to a severity level and optional message.
type HWSeverityMapping struct {
	Severity string `json:"severity"`
	Message  string `json:"message,omitempty"`
}

// PrometheusExporter exposes metrics on a Prometheus-compatible HTTP endpoint.
type PrometheusExporter struct {
	Endpoint string `json:"endpoint"`
}

// OTelService is the top-level service configuration block.
type OTelService struct {
	Telemetry OTelTelemetry `json:"telemetry"`
	Pipelines OTelPipelines `json:"pipelines"`
}

// OTelTelemetry configures the collector's own telemetry output.
type OTelTelemetry struct {
	Logs OTelLogs `json:"logs"`
}

// OTelLogs configures the log level for the collector itself.
type OTelLogs struct {
	Level string `json:"level"`
}

// OTelPipelines defines the data pipelines in the collector.
type OTelPipelines struct {
	Metrics PipelineMetrics `json:"metrics"`
	Logs    *PipelineLogs   `json:"logs,omitempty"`
}

// PipelineMetrics lists the components forming the metrics pipeline.
type PipelineMetrics struct {
	Receivers  []string `json:"receivers"`
	Processors []string `json:"processors"`
	Exporters  []string `json:"exporters"`
}

// PipelineLogs lists the components forming the logs pipeline.
type PipelineLogs struct {
	Receivers  []string `json:"receivers"`
	Processors []string `json:"processors,omitempty"`
	Exporters  []string `json:"exporters"`
}
