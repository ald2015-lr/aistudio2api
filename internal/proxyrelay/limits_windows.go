package proxyrelay

// defaultTotalConnections 在 Windows 上用常量：没有 RLIMIT_NOFILE
func defaultTotalConnections() int {
	return 16384
}
