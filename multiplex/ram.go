package multiplex

// DefaultMaxRSS is the per-child RSS limit used when --lsp-max-rss-mb is
// not given: min(3072 MiB, 15% of total RAM), in bytes. With RAM unknown
// (total <= 0) it is 3072 MiB.
func DefaultMaxRSS(total int64) int64 {
	const cap = int64(3072) << 20
	if total <= 0 {
		return cap
	}
	return min(cap, total/100*15)
}
