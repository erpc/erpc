package util

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

func EvmNetworkId(chainId interface{}) string {
	return fmt.Sprintf("evm:%d", chainId)
}

// SvmNetworkId derives the canonical "svm:..." network ID. When chain is
// empty or "solana", the format stays "svm:<cluster>" — preserving every
// pre-multi-chain config's network ID and cache key. For any other chain
// the format is "svm:<chain>:<cluster>" so forks (Fogo, Eclipse, custom)
// can coexist with Solana in a single eRPC instance.
func SvmNetworkId(chain, cluster string) string {
	if chain == "" || chain == "solana" {
		return "svm:" + cluster
	}
	return "svm:" + chain + ":" + cluster
}

var validIdentifierRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func IsValidIdentifier(s string) bool {
	return validIdentifierRegex.MatchString(s)
}

// MaxNetworkIdLength bounds a network id accepted off the wire. Real ids are
// far shorter ("evm:11155111", "svm:eclipse:mainnet-beta").
const MaxNetworkIdLength = 64

// IsValidNetworkId reports whether s is a network id eRPC will lazily
// bootstrap a network for. Each accepted id gets its own BootstrapTask,
// NetworkConfig and `network` label, so an "evm:" chain id must use its one
// canonical decimal spelling: "evm:007", "evm:-1" and "evm:+1" are not aliases
// for a real network.
func IsValidNetworkId(s string) bool {
	if len(s) > MaxNetworkIdLength {
		return false
	}
	if strings.HasPrefix(s, "evm:") {
		return isCanonicalChainId(s[4:])
	}
	if strings.HasPrefix(s, "svm:") {
		// Two accepted shapes: "svm:<cluster>" (implicit solana, back-compat)
		// and "svm:<chain>:<cluster>" (explicit chain prefix). Validate each
		// segment as an identifier so "svm::" or trailing-colon nonsense is
		// rejected.
		rest := s[4:]
		if rest == "" {
			return false
		}
		for _, segment := range strings.Split(rest, ":") {
			if segment == "" {
				return false
			}
			for _, r := range segment {
				if !(r == '-' || r == '_' || r == '.' ||
					(r >= 'a' && r <= 'z') ||
					(r >= 'A' && r <= 'Z') ||
					(r >= '0' && r <= '9')) {
					return false
				}
			}
		}
		// Reject more than 2 segments — no use case for svm:a:b:c today.
		if strings.Count(rest, ":") > 1 {
			return false
		}
		return true
	}
	return false
}

// isCanonicalChainId reports whether s is the canonical decimal spelling of a
// positive int64 chain id: digits only, no sign, no leading zero.
func isCanonicalChainId(s string) bool {
	if s == "" || s[0] == '0' {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	// Only overflow is left for ParseInt to catch.
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}

var counters = make(map[string]int)
var countersMutex = sync.Mutex{}

func IncrementAndGetIndex(parts ...string) string {
	countersMutex.Lock()
	defer countersMutex.Unlock()
	counterKey := strings.Join(parts, "</@/>")
	if _, ok := counters[counterKey]; !ok {
		counters[counterKey] = 0
	}
	counters[counterKey]++
	return strconv.Itoa(counters[counterKey])
}
