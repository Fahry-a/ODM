// SPDX-License-Identifier: MIT
//
// smart.go is the decision matrix for the smart profile: after probing the
// server (range support, size) and checking HTTP/2 readiness, ODM picks the
// most suitable engine per file. The rules, in evaluation order:
//
//	 1. no range support / sizeless / single-stream → odm (fixed chunks are
//	    meaningless without ranged GETs; single whole-file GET)
//	 2. TotalSize < 8 MiB                    → odm (split overhead not worth it)
//	 3. server doesn't speak HTTP/2          → odm (h2 profiles gain nothing)
//	 4. Conns <= 2                           → odm (no parallelism to split)
//	 5. TotalSize >= 256 MiB && Conns >= 6   → both (large file, wide budget:
//	    two engines side by side)
//	 6. otherwise                            → aria2c (h2 streams)
//
// Each row returns a human reason for the confirmation prompt / log.

package download

// ServerCapabilities are the inputs the smart decision needs, all known by
// the time the CLI probe pass finishes.
type ServerCapabilities struct {
	TotalSize     int64
	SupportsRange bool
	SingleStream  bool
	HTTP2Ready    bool // client h2-enabled AND the server negotiated h2
	Conns         int
}

// ChooseProfile returns the best engine profile + a short reason.
func ChooseProfile(c ServerCapabilities) (profile, reason string) {
	switch {
	case c.SingleStream || !c.SupportsRange || c.TotalSize <= 0:
		return "odm", "no range support"
	case c.TotalSize < 8<<20:
		return "odm", "small file"
	case !c.HTTP2Ready:
		return "odm", "no h2"
	case c.Conns <= 2:
		return "odm", "low conns"
	case c.TotalSize >= 256<<20 && c.Conns >= 6:
		return "both", "large+wide"
	default:
		return "aria2c", "default h2"
	}
}
