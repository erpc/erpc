package evm

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/consensus"
	"github.com/rs/zerolog/log"
)

// Integrity catches (rejects and soft-flags) are rare and each one is
// forensic gold: metrics carry the counts, this archive carries the WHAT —
// upstream, check, verbatim reason, and the offending response body — as
// durable JSONL via integrity.misbehaviorsDestination (same file/S3 shape as
// the consensus policy's misbehaviorsDestination).

// maxExportBody caps the archived response body per record.
const maxExportBody = 64 * 1024

// integrityExporters caches one exporter per network, created lazily on the
// first catch. The slot is cached even when unconfigured (nil exporter) so
// the config lookup happens once.
var integrityExporters sync.Map // network id -> *integrityExporterSlot

type integrityExporterSlot struct{ exp consensus.MisbehaviorExporter }

func networkIntegrityExporter(n common.Network) consensus.MisbehaviorExporter {
	key := n.Id()
	if v, ok := integrityExporters.Load(key); ok {
		return v.(*integrityExporterSlot).exp
	}
	slot := &integrityExporterSlot{}
	if ic := n.Config().Integrity; ic != nil && ic.MisbehaviorsDestination != nil {
		l := log.With().Str("component", "integrityExport").Str("network", n.Id()).Logger()
		slot.exp = consensus.NewMisbehaviorExporter(ic.MisbehaviorsDestination, &l)
	}
	actual, _ := integrityExporters.LoadOrStore(key, slot)
	return actual.(*integrityExporterSlot).exp
}

// integrityCatchRecord is one archived catch. Reason is the verbatim
// expected-vs-actual explanation; Response is the offending body (capped).
type integrityCatchRecord struct {
	Timestamp string `json:"ts"`
	Project   string `json:"project"`
	Network   string `json:"network"`
	Upstream  string `json:"upstream"`
	Vendor    string `json:"vendor"`
	Method    string `json:"method"`
	Check     string `json:"check"`
	Class     string `json:"class"`
	Verdict   string `json:"verdict"` // reject | soft_flag
	Finality  string `json:"finality"`
	Reason    string `json:"reason"`
	Response  string `json:"response,omitempty"`
}

// exportIntegrityCatch archives one catch; best-effort (failures are logged,
// never affect the request path).
func exportIntegrityCatch(ctx context.Context, n common.Network, u common.Upstream, rs *common.NormalizedResponse, method, verdict, check, class, finality, reason string) {
	exp := networkIntegrityExporter(n)
	if exp == nil {
		return
	}
	rec := integrityCatchRecord{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Project:   n.ProjectId(),
		Network:   n.Label(),
		Upstream:  u.Id(),
		Vendor:    u.VendorName(),
		Method:    method,
		Check:     check,
		Class:     class,
		Verdict:   verdict,
		Finality:  finality,
		Reason:    reason,
	}
	if rs != nil {
		if jrr, err := rs.JsonRpcResponse(ctx); err == nil && jrr != nil {
			body := jrr.GetResultBytes()
			if len(body) > maxExportBody {
				body = body[:maxExportBody]
			}
			rec.Response = string(body)
		}
	}
	line, err := common.SonicCfg.Marshal(rec)
	if err != nil {
		return
	}
	if err := exp.AppendWithMetadata(line, method, n.Id()); err != nil {
		log.Warn().Err(err).Str("network", n.Id()).Str("check", check).
			Msg("integrity: failed to archive catch to misbehaviorsDestination")
	}
}

// CloseIntegrityExporters flushes and closes every per-network misbehaviour
// exporter. Called on graceful shutdown: the S3 exporter batches records and
// only writes them on its flush interval, so without this a pod roll drops
// everything buffered since the last flush — exactly the catches an operator
// most wants to adjudicate, since a roll usually follows the incident.
func CloseIntegrityExporters() {
	integrityExporters.Range(func(_, v any) bool {
		slot, ok := v.(*integrityExporterSlot)
		if !ok || slot.exp == nil {
			return true
		}
		if c, ok := slot.exp.(io.Closer); ok {
			_ = c.Close()
		}
		return true
	})
}
