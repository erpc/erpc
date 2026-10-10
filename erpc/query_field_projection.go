package erpc

import (
	"github.com/blockchain-data-standards/manifesto/evm"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// projectQueryPage returns a copy of a shim page whose objects keep only the
// fields req selects, for gRPC clients (the JSON renderer writes only
// selected fields). A nil selection keeps every field. It copies because
// objects point into their block's header (a transaction's blockNumber and
// blockTimestamp, a trace's blockTimestamp), and the blocks relation holds
// that header itself.
func projectQueryPage(req, page proto.Message) proto.Message {
	page = proto.Clone(page)
	switch r := req.(type) {
	case *evm.QueryBlocksRequest:
		p := page.(*evm.QueryBlocksResponse)
		projectEach(p.Blocks, r.GetBlockFields())
	case *evm.QueryTransactionsRequest:
		p := page.(*evm.QueryTransactionsResponse)
		projectEach(p.Transactions, r.GetTransactionFields())
		projectEach(p.Blocks, r.GetBlockFields())
	case *evm.QueryLogsRequest:
		p := page.(*evm.QueryLogsResponse)
		projectEach(p.Logs, r.GetLogFields())
		projectEach(p.Transactions, r.GetTransactionFields())
		projectEach(p.Blocks, r.GetBlockFields())
	case *evm.QueryTracesRequest:
		p := page.(*evm.QueryTracesResponse)
		projectEach(p.Traces, r.GetTraceFields())
		projectEach(p.Transactions, r.GetTransactionFields())
		projectEach(p.Blocks, r.GetBlockFields())
	case *evm.QueryTransfersRequest:
		p := page.(*evm.QueryTransfersResponse)
		projectEach(p.Transfers, r.GetTransferFields())
		projectEach(p.Transactions, r.GetTransactionFields())
		projectEach(p.Blocks, r.GetBlockFields())
	}
	return page
}

// selectionAliases maps a selection field name to the object field it
// selects where the two names differ (MIP-16 names the gas limit "gas").
var selectionAliases = map[protoreflect.FullName]map[protoreflect.Name]protoreflect.Name{
	(&evm.Transaction{}).ProtoReflect().Descriptor().FullName(): {"gas": "gasLimit"},
}

// projectEach clears, on each object, every field sel names but does not
// select. A nil selection keeps every field.
func projectEach[T proto.Message, S proto.Message](objects []T, sel S) {
	selMsg := sel.ProtoReflect()
	if !selMsg.IsValid() || len(objects) == 0 {
		return
	}
	selFields := selMsg.Descriptor().Fields()
	objDesc := objects[0].ProtoReflect().Descriptor()
	aliases := selectionAliases[objDesc.FullName()]
	var cleared []protoreflect.FieldDescriptor
	for i := range selFields.Len() {
		sf := selFields.Get(i)
		if selMsg.Get(sf).Bool() {
			continue
		}
		name := sf.Name()
		if alias, ok := aliases[name]; ok {
			name = alias
		}
		if of := objDesc.Fields().ByName(name); of != nil {
			cleared = append(cleared, of)
		}
	}
	for _, obj := range objects {
		m := obj.ProtoReflect()
		for _, of := range cleared {
			m.Clear(of)
		}
	}
}
