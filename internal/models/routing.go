package models

// EndpointHealth is an aggregate of recent request-log rows for a single
// (provider, model) pair. It is the health signal the provider router uses to
// gate candidate endpoints — computed over the request log, not captured
// separately.
type EndpointHealth struct {
	Provider     string  `json:"provider"     db:"provider"`
	Model        string  `json:"model"        db:"model"`
	SampleCount  int64   `json:"sampleCount"  db:"sample_count"`
	AvgLatencyMs float64 `json:"avgLatencyMs" db:"avg_latency_ms"`
	ErrorRate    float64 `json:"errorRate"    db:"error_rate"`
}
