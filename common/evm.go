package common

type EvmNodeType string

const (
	EvmNodeTypeFull      EvmNodeType = "full"
	EvmNodeTypeArchive   EvmNodeType = "archive"
	EvmNodeTypeSequencer EvmNodeType = "sequencer"
	EvmNodeTypeExecution EvmNodeType = "execution"
)

type EvmBlockTracker interface {
	LatestBlock() uint64
	FinalizedBlock() uint64
}
