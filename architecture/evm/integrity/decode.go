package integrity

import "github.com/erpc/erpc/common"

// Lowercased method names this package understands. Checks reference these and
// the engine matches the request method (lowercased) against them.
const (
	MethodGetBlockByNumber      = "eth_getblockbynumber"
	MethodGetBlockByHash        = "eth_getblockbyhash"
	MethodGetBlockReceipts      = "eth_getblockreceipts"
	MethodGetTransactionReceipt = "eth_gettransactionreceipt"
	MethodGetLogs               = "eth_getlogs"
	MethodGetTransactionByHash  = "eth_gettransactionbyhash"
)

// Log is the lightweight view of a single EVM log. Only fields the checks read
// are modeled; everything else is ignored on decode.
type Log struct {
	Address          string   `json:"address"`
	Topics           []string `json:"topics"`
	Data             string   `json:"data"`
	BlockHash        string   `json:"blockHash"`
	BlockNumber      string   `json:"blockNumber"`
	TransactionHash  string   `json:"transactionHash"`
	TransactionIndex string   `json:"transactionIndex"`
	LogIndex         string   `json:"logIndex"`
	Removed          bool     `json:"removed"`
}

// Receipt is the lightweight view of a transaction receipt.
type Receipt struct {
	BlockHash         string `json:"blockHash"`
	BlockNumber       string `json:"blockNumber"`
	TransactionHash   string `json:"transactionHash"`
	TransactionIndex  string `json:"transactionIndex"`
	LogsBloom         string `json:"logsBloom"`
	ContractAddress   string `json:"contractAddress"`
	Status            string `json:"status"`
	CumulativeGasUsed string `json:"cumulativeGasUsed"`
	GasUsed           string `json:"gasUsed"`
	Type              string `json:"type"`
	Logs              []Log  `json:"logs"`
}

// Tx is the lightweight view of a transaction.
type Tx struct {
	Hash             string `json:"hash"`
	From             string `json:"from"`
	To               string `json:"to"`
	Gas              string `json:"gas"`
	BlockHash        string `json:"blockHash"`
	BlockNumber      string `json:"blockNumber"`
	TransactionIndex string `json:"transactionIndex"`
}

// Header is the lightweight view of a block header plus its raw transactions
// (which may be hash strings or full objects, hence []any).
type Header struct {
	Hash             string `json:"hash"`
	ParentHash       string `json:"parentHash"`
	StateRoot        string `json:"stateRoot"`
	TransactionsRoot string `json:"transactionsRoot"`
	ReceiptsRoot     string `json:"receiptsRoot"`
	LogsBloom        string `json:"logsBloom"`
	Number           string `json:"number"`
	RawTransactions  []any  `json:"transactions"`
}

// Decoded is a response result decoded once into normalized EVM views. Each
// accessor decodes lazily on first use and caches the result, so a check that
// needs logs and a check that needs receipts share a single parse. Decode
// errors yield empty views (an absent/garbled section is not, by itself, a
// violation — the schema-conformance check owns that judgment).
type Decoded struct {
	method string
	raw    []byte

	header   *Header
	txs      []Tx
	receipts []Receipt
	logs     []Log

	headerDone, txsDone, receiptsDone, logsDone bool
}

func newDecoded(method string, raw []byte) *Decoded {
	return &Decoded{method: method, raw: raw}
}

// Header returns the block header for block methods, or nil otherwise.
func (d *Decoded) Header() *Header {
	if d.headerDone {
		return d.header
	}
	d.headerDone = true
	switch d.method {
	case MethodGetBlockByNumber, MethodGetBlockByHash:
		var h Header
		if err := common.SonicCfg.Unmarshal(d.raw, &h); err == nil {
			d.header = &h
		}
	}
	return d.header
}

// Receipts returns the receipts carried by the response: the array for
// eth_getBlockReceipts, or a one-element slice for eth_getTransactionReceipt.
func (d *Decoded) Receipts() []Receipt {
	if d.receiptsDone {
		return d.receipts
	}
	d.receiptsDone = true
	switch d.method {
	case MethodGetBlockReceipts:
		_ = common.SonicCfg.Unmarshal(d.raw, &d.receipts)
	case MethodGetTransactionReceipt:
		var r Receipt
		if err := common.SonicCfg.Unmarshal(d.raw, &r); err == nil {
			d.receipts = []Receipt{r}
		}
	}
	return d.receipts
}

// Logs returns the logs carried by the response: the array for eth_getLogs, or
// the logs flattened across receipts for the receipt methods.
func (d *Decoded) Logs() []Log {
	if d.logsDone {
		return d.logs
	}
	d.logsDone = true
	switch d.method {
	case MethodGetLogs:
		_ = common.SonicCfg.Unmarshal(d.raw, &d.logs)
	case MethodGetBlockReceipts, MethodGetTransactionReceipt:
		for i := range d.Receipts() {
			d.logs = append(d.logs, d.receipts[i].Logs...)
		}
	}
	return d.logs
}

// Transactions returns the full transaction objects available in the response:
// the block's transactions when present as objects (eth_getBlockByNumber with
// full=true), or a one-element slice for eth_getTransactionByHash. Hash-only
// block transactions yield no entries.
func (d *Decoded) Transactions() []Tx {
	if d.txsDone {
		return d.txs
	}
	d.txsDone = true
	switch d.method {
	case MethodGetTransactionByHash:
		var t Tx
		if err := common.SonicCfg.Unmarshal(d.raw, &t); err == nil {
			d.txs = []Tx{t}
		}
	case MethodGetBlockByNumber, MethodGetBlockByHash:
		h := d.Header()
		if h == nil {
			return nil
		}
		for _, raw := range h.RawTransactions {
			obj, ok := raw.(map[string]any)
			if !ok {
				continue // hash-only entry; nothing to validate at tx level
			}
			b, err := common.SonicCfg.Marshal(obj)
			if err != nil {
				continue
			}
			var t Tx
			if err := common.SonicCfg.Unmarshal(b, &t); err == nil {
				d.txs = append(d.txs, t)
			}
		}
	}
	return d.txs
}
