package common

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A network that sets any evm field (in practice, chainId) gets
// networkDefaults.evm only through the field-by-field merge in
// NetworkConfig.SetDefaults; the whole block is copied only when the network
// has no evm block at all.
//
// evmFieldsNotInherited are the EvmNetworkConfig fields networkDefaults.evm
// deliberately does not fill in. Every other field must be merged; a new
// field without a merge line fails the test below.
var evmFieldsNotInherited = map[string]string{
	"ChainId":                     "identifies the network; each network sets its own",
	"MaxFutureBlockRetryDistance": "deprecated; SetDefaults warns and ignores it",
}

func TestNetworkDefaults_EveryEvmFieldIsInheritedByANetworkWithItsOwnEvmBlock(t *testing.T) {
	defaults := &EvmNetworkConfig{}
	fillSentinel(reflect.ValueOf(defaults).Elem())
	n := &NetworkConfig{Architecture: ArchitectureEvm, Evm: &EvmNetworkConfig{ChainId: 369}}

	require.NoError(t, n.SetDefaults(nil, &NetworkDefaults{Evm: defaults}))

	got, want := reflect.ValueOf(n.Evm).Elem(), reflect.ValueOf(defaults).Elem()
	for i := 0; i < want.NumField(); i++ {
		name := want.Type().Field(i).Name
		if _, skip := evmFieldsNotInherited[name]; skip || !want.Type().Field(i).IsExported() {
			continue
		}
		assert.True(t, reflect.DeepEqual(got.Field(i).Interface(), want.Field(i).Interface()),
			"networkDefaults.evm.%s is not inherited by a network with its own evm block: add it to the merge in NetworkConfig.SetDefaults, or to evmFieldsNotInherited with a reason", name)
	}
}

// fillSentinel sets every settable field to a value that no system default
// produces: odd numbers, the string "sentinel", one-element slices and maps,
// and false behind a *bool (the system defaults set true).
func fillSentinel(v reflect.Value) {
	switch v.Kind() {
	case reflect.Ptr:
		if v.Type().Elem().Kind() == reflect.Bool {
			f := false
			v.Set(reflect.ValueOf(&f))
			return
		}
		nv := reflect.New(v.Type().Elem())
		fillSentinel(nv.Elem())
		v.Set(nv)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).CanSet() {
				fillSentinel(v.Field(i))
			}
		}
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(77)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(77)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(0.0777)
	case reflect.String:
		v.SetString("sentinel")
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillSentinel(s.Index(0))
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fillSentinel(k)
		fillSentinel(e)
		m.SetMapIndex(k, e)
		v.Set(m)
	}
}
