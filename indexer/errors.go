package indexer

import "fmt"

// errNetworkNotRegistered is returned by indexer calls that reference a
// networkId never passed to RegisterNetwork. It's a programmer error at
// wiring time — wrapped rather than a sentinel because callers have no
// useful action beyond failing the startup path.
func errNetworkNotRegistered(networkId string) error {
	return fmt.Errorf("indexer: network %q not registered", networkId)
}
