package common

type EvmNodeType string

const (
	EvmNodeTypeFull      EvmNodeType = "full"
	EvmNodeTypeArchive   EvmNodeType = "archive"
	EvmNodeTypeSequencer EvmNodeType = "sequencer"
	EvmNodeTypeExecution EvmNodeType = "execution"
)
