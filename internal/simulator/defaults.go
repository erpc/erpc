package simulator

// defaultKnobsFor synthesizes plausible behavioural defaults for a
// newly-registered upstream based on its identity-side metadata
// (vendor name, tags). Operators can re-tune any of these live from
// the UI; this just gives them a sensible starting point so the
// simulator's seed pool doesn't all behave identically.
//
// Defaults are mild-bias: premium tier gets a low base latency,
// fallback tier gets visible jitter + a non-zero base error rate. None
// of these are meant to model a specific provider — operators looking
// for that should tune the knobs themselves.
func defaultKnobsFor(id UpstreamIdentity) UpstreamKnobs {
	k := UpstreamKnobs{
		ID:               id.ID,
		Vendor:           id.Vendor,
		Tags:             append([]string(nil), id.Tags...),
		BaseLatencyMs:    50,
		JitterMs:         20,
		ErrorRate:        0.01,
		TimeoutRate:      0.002,
		ThrottleRate:     0.002,
		BlockLag:         0,
		BlockTimeMs:      12_000,
		Available:        true,
		DataAvailability: 1.0, // baseline; per-method baselines still apply
	}
	for _, t := range id.Tags {
		switch t {
		case "arch:svm":
			// Solana-shaped upstream: 400ms slots. BlockLag then means
			// "slots behind" and blockNumberLagAbove(...) reads slot lag.
			k.BlockTimeMs = 400
		case "premium":
			k.BaseLatencyMs = 35
			k.JitterMs = 12
			k.ErrorRate = 0.003
		case "dedicated":
			k.BaseLatencyMs = 45
			k.JitterMs = 18
			k.ErrorRate = 0.005
		case "tier:fallback":
			k.BaseLatencyMs = 180
			k.JitterMs = 110
			k.ErrorRate = 0.080
			k.TimeoutRate = 0.020
			k.ThrottleRate = 0.040
			k.BlockLag = 4
		}
	}
	switch id.Vendor {
	case "self-hosted":
		k.BaseLatencyMs = 28
		k.JitterMs = 12
		k.ErrorRate = 0.02
	case "public":
		// already biased by tier:fallback for most public endpoints,
		// but if the operator left tags empty give them the same hint.
		if k.ErrorRate < 0.05 {
			k.ErrorRate = 0.08
			k.BaseLatencyMs = 180
			k.JitterMs = 110
			k.BlockLag = 4
		}
	}
	return k
}
