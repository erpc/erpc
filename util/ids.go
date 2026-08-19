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

// MaxNetworkIdLength bounds a network id accepted off the wire. Every real id
// is far shorter ("evm:11155111", "svm:eclipse:mainnet-beta"); the cap stops
// an arbitrarily long path segment from becoming a Prometheus label value.
const MaxNetworkIdLength = 64

// IsValidNetworkId reports whether s is a network id eRPC will lazily
// bootstrap a network for.
//
// Every accepted id costs a permanent BootstrapTask in the project's
// initializer, a lazily-created NetworkConfig appended to the project config,
// and a distinct `network` label value across the metric families — so the
// accepted set must be one id per real network, not one per spelling of it.
// For "evm:" that means the CANONICAL decimal chain id only: strconv.Atoi
// used to accept "evm:007", "evm:-1" and "evm:+1" as distinct-but-equivalent
// ids, which is an unbounded family of aliases for chain 7 / an impossible
// chain.
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

// isCanonicalChainId reports whether s is the one canonical decimal spelling
// of a positive chain id: digits only, no sign, no leading zero, and within
// int64 (what EvmNetworkConfig.ChainId holds).
func isCanonicalChainId(s string) bool {
	// Digits only, no sign, no leading zero — so exactly one spelling of any
	// given chain id reaches the network registry.
	if s == "" || s[0] == '0' {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	// Overflow check only; the scan above already ruled out everything else
	// ParseInt would tolerate. Reached only on a network-registry cache miss.
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
