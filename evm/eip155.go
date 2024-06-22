package evm

import "fmt"

func EIP155(chainId any) string {
	return fmt.Sprintf("eip155:%s", chainId)
}
