package common

import (
	"fmt"
	"testing"

	"github.com/erpc/erpc/util"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func TestUpstreamValidate_ProbeMode(t *testing.T) {
	cases := []struct {
		name         string
		routing      *UpstreamRoutingConfig
		invalidValue string
	}{
		{"routing omitted", nil, ""},
		{"probe omitted", &UpstreamRoutingConfig{}, ""},
		{"on", &UpstreamRoutingConfig{Probe: ProbeModeOn}, ""},
		{"off", &UpstreamRoutingConfig{Probe: ProbeModeOff}, ""},
		{"false", &UpstreamRoutingConfig{Probe: "false"}, "false"},
		{"true", &UpstreamRoutingConfig{Probe: "true"}, "true"},
		{"unknown", &UpstreamRoutingConfig{Probe: "disabled"}, "disabled"},
		{"case sensitive", &UpstreamRoutingConfig{Probe: "OFF"}, "OFF"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &UpstreamConfig{
				Id: "rpc-main", Endpoint: "http://localhost:8545",
				Routing: tc.routing,
			}
			err := u.Validate(&Config{}, false)
			if tc.invalidValue == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, "rpc-main")
			require.ErrorContains(t, err, "routing.probe")
			require.ErrorContains(t, err, tc.invalidValue)
		})
	}
}

func TestLoadConfig_ProbeMode(t *testing.T) {
	// Cover all entry points: provider-only projects otherwise defer copying
	// upstreamDefaults until background bootstrap, after config validation.
	locations := []struct {
		name     string
		template string
		context  string
	}{
		{"upstream", `
projects:
  - id: main
    upstreams:
      - id: rpc-main
        endpoint: http://localhost:8545
        routing:
          probe: %s
`, "rpc-main"},
		{"upstream defaults", `
projects:
  - id: main
    upstreamDefaults:
      routing:
        probe: %s
    providers:
      - id: public
        vendor: repository
`, "upstreamDefaults"},
		{"provider override", `
projects:
  - id: main
    providers:
      - id: public
        vendor: repository
        overrides:
          "*":
            id: rpc-override
            routing:
              probe: %s
`, "rpc-override"},
	}
	values := []struct {
		yaml  string
		valid bool
		mode  ProbeMode
	}{
		{`""`, true, ""},
		{"on", true, ProbeModeOn},
		{"off", true, ProbeModeOff},
		{`"on"`, true, ProbeModeOn},
		{`"off"`, true, ProbeModeOff},
		{"false", false, ""},
		{"true", false, ""},
		{`"false"`, false, ""},
		{"disabled", false, ""},
		{"OFF", false, ""},
		{"0", false, ""},
	}
	for _, location := range locations {
		for _, value := range values {
			t.Run(location.name+"/"+value.yaml, func(t *testing.T) {
				fs := afero.NewMemMapFs()
				src := fmt.Sprintf(location.template, value.yaml)
				require.NoError(t, afero.WriteFile(fs, "erpc.yaml", []byte(src), 0o644))
				cfg, err := LoadConfig(fs, "erpc.yaml", &DefaultOptions{})
				if value.valid {
					require.NoError(t, err)
					var routing *UpstreamRoutingConfig
					switch location.name {
					case "upstream":
						routing = cfg.Projects[0].Upstreams[0].Routing
					case "upstream defaults":
						routing = cfg.Projects[0].UpstreamDefaults.Routing
					case "provider override":
						routing = cfg.Projects[0].Providers[0].Overrides["*"].Routing
					}
					require.NotNil(t, routing)
					require.Equal(t, value.mode, routing.Probe)
					return
				}
				require.ErrorContains(t, err, "routing.probe")
				require.ErrorContains(t, err, location.context)
			})
		}
	}
}
