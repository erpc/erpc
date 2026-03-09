package evm

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/bytedance/sonic/ast"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

const getLogsMaxSizeField = "maxSize"

func extractGetLogsMaxDataBytes(filter map[string]interface{}) (int64, error) {
	if filter == nil {
		return 0, nil
	}

	raw, ok := filter[getLogsMaxSizeField]
	if !ok || raw == nil {
		return 0, nil
	}

	switch v := raw.(type) {
	case int:
		if v < 0 {
			return 0, fmt.Errorf("%s must be greater than or equal to 0", getLogsMaxSizeField)
		}
		return int64(v), nil
	case int64:
		if v < 0 {
			return 0, fmt.Errorf("%s must be greater than or equal to 0", getLogsMaxSizeField)
		}
		return v, nil
	case float64:
		if v < 0 {
			return 0, fmt.Errorf("%s must be greater than or equal to 0", getLogsMaxSizeField)
		}
		if math.Trunc(v) != v {
			return 0, fmt.Errorf("%s must be an integer", getLogsMaxSizeField)
		}
		return int64(v), nil
	case string:
		if v == "" {
			return 0, nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer: %w", getLogsMaxSizeField, err)
		}
		if n < 0 {
			return 0, fmt.Errorf("%s must be greater than or equal to 0", getLogsMaxSizeField)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("%s must be a number", getLogsMaxSizeField)
	}
}

func extractGetLogsPayloadLimitFromRequest(ctx context.Context, rq *common.NormalizedRequest) (int64, error) {
	jrq, err := rq.JsonRpcRequest(ctx)
	if err != nil {
		return 0, err
	}

	jrq.RLock()
	defer jrq.RUnlock()

	if len(jrq.Params) == 0 {
		return 0, nil
	}
	filter, ok := jrq.Params[0].(map[string]interface{})
	if !ok {
		return 0, nil
	}

	return extractGetLogsMaxDataBytes(filter)
}

func filterGetLogsResponseByDataLimit(jrr *common.JsonRpcResponse, filter *getLogsFilter) (int, error) {
	if jrr == nil || filter == nil || filter.maxDataBytes <= 0 {
		return 0, nil
	}

	resultBytes := jrr.GetResultBytes()
	if len(resultBytes) == 0 {
		return 0, nil
	}

	root := ast.NewRawConcurrentRead(util.B2Str(resultBytes))
	if err := root.LoadAll(); err != nil {
		return 0, err
	}
	iter, err := root.Values()
	if err != nil {
		return 0, err
	}

	var buf bytes.Buffer
	buf.WriteByte('[')

	first := true
	dropped := 0
	var lg ast.Node
	for iter.Next(&lg) {
		if !filter.matchesLog(&lg) {
			dropped++
			continue
		}

		raw, err := lg.Raw()
		if err != nil {
			return 0, err
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		buf.WriteString(raw)
	}

	buf.WriteByte(']')
	if dropped == 0 {
		return 0, nil
	}

	jrr.SetResult(append([]byte(nil), buf.Bytes()...))
	return dropped, nil
}

func logDataExceedsLimit(data string, maxDataBytes int64) bool {
	if maxDataBytes <= 0 || data == "" {
		return false
	}

	if strings.HasPrefix(data, "0x") || strings.HasPrefix(data, "0X") {
		hexLen := len(data) - 2
		if hexLen <= 0 {
			return false
		}
		byteLen := hexLen / 2
		if hexLen%2 != 0 {
			byteLen++
		}
		return int64(byteLen) > maxDataBytes
	}

	return int64(len(data)) > maxDataBytes
}
