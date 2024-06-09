package upstream

import (
	"time"

	"github.com/rs/zerolog"
)

type UpstreamMetrics struct {
	P90Latency     float64   `json:"p90Latency"`
	ErrorsTotal    float64   `json:"errorsTotal"`
	ThrottledTotal float64   `json:"throttledTotal"`
	RequestsTotal  float64   `json:"requestsTotal"`
	BlocksLag      float64   `json:"blocksLag"`
	LastCollect    time.Time `json:"lastCollect"`
}

func (c *UpstreamMetrics) MarshalZerologObject(e *zerolog.Event) {
	e.Float64("p90Latency", c.P90Latency).
		Float64("errorsTotal", c.ErrorsTotal).
		Float64("requestsTotal", c.RequestsTotal).
		Float64("throttledTotal", c.ThrottledTotal).
		Float64("blocksLag", c.BlocksLag).
		Time("lastCollect", c.LastCollect)
}
