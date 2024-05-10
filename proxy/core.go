package proxy

import (
	"net/http"

	"github.com/flair-sdk/erpc/upstream"
)

type ProxyCore struct {
	upstreamOrchestrator *upstream.UpstreamOrchestrator
}

func NewProxyCore(upstreamOrchestrator *upstream.UpstreamOrchestrator) *ProxyCore {
	return &ProxyCore{
		upstreamOrchestrator: upstreamOrchestrator,
	}
}

// Forward a request for a project/network to an upstream selected by the orchestrator.
func (p *ProxyCore) Forward(project string, network string, w http.ResponseWriter, r *http.Request) {
	upstream, err := p.upstreamOrchestrator.FindBestUpstream(project, network)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}

	w.Write([]byte("Found upstream: " + upstream + " for project: " + project + " and network: " + network))
}
