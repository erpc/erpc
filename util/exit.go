package util

import "os"

var OsExit = os.Exit

var (
	ExitCodeERPCStartFailed  = 1001
	ExitCodeHttpServerFailed = 1002
)
