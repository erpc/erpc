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

func IsValidNetworkId(s string) bool {
	if strings.HasPrefix(s, "evm:") {
		_, err := strconv.Atoi(s[4:])
		return err == nil
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
