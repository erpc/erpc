package util

import (
	"fmt"
	"regexp"
)

func EvmNetworkId(chainId interface{}) string {
	return fmt.Sprintf("evm:%d", chainId)
}

var validIdentifierRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func IsValidIdentifier(s string) bool {
	return validIdentifierRegex.MatchString(s)
}
