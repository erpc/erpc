package upstream

import (
	"time"

	"github.com/rs/zerolog"
)

type UpstreamMetrics struct {
	LatencySecs    float64   `json:"latencySecs"`
	ErrorsTotal    float64   `json:"errorsTotal"`
	ThrottledTotal float64   `json:"throttledTotal"`
	RequestsTotal  float64   `json:"requestsTotal"`
	LastCollect    time.Time `json:"lastCollect"`
}

func (c *UpstreamMetrics) MarshalZerologObject(e *zerolog.Event) {
	e.Float64("latencySecs", c.LatencySecs).
		Float64("errorsTotal", c.ErrorsTotal).
		Float64("requestsTotal", c.RequestsTotal).
		Float64("throttledTotal", c.ThrottledTotal).
		Time("lastCollect", c.LastCollect)
}
