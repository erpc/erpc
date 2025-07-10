package common

import (
	"reflect"

	"github.com/bytedance/sonic"
	"github.com/bytedance/sonic/option"
)

var SonicCfg sonic.API

func init() {
	err := sonic.Pretouch(
		reflect.TypeOf(NormalizedResponse{}),
		option.WithCompileMaxInlineDepth(1),
	)
	if err != nil {
		panic(err)
	}
	err = sonic.Pretouch(
		reflect.TypeOf(JsonRpcResponse{}),
		option.WithCompileMaxInlineDepth(1),
	)
	if err != nil {
		panic(err)
	}
	err = sonic.Pretouch(
		reflect.TypeOf(JsonRpcRequest{}),
		option.WithCompileMaxInlineDepth(1),
	)
	if err != nil {
		panic(err)
	}
	err = sonic.Pretouch(
		reflect.TypeOf(NormalizedRequest{}),
		option.WithCompileMaxInlineDepth(1),
	)
	if err != nil {
		panic(err)
	}
	err = sonic.Pretouch(
		reflect.TypeOf(BaseError{}),
		option.WithCompileMaxInlineDepth(1),
	)
	if err != nil {
		panic(err)
	}
	SonicCfg = sonic.Config{
		CopyString:              true,
		NoNullSliceOrMap:        true,
		NoQuoteTextMarshaler:    true,
		NoValidateJSONMarshaler: true,
		NoValidateJSONSkip:      true,
		EscapeHTML:              false,
		SortMapKeys:             false,
		CompactMarshaler:        true,
		ValidateString:          false,
	}.Froze()
}
