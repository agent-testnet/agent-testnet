//go:build !linux

package proxy

import (
	"errors"
	"net"
)

// errOrigDstUnsupported is returned by origDst on non-Linux platforms.
// The MITM proxy needs SO_ORIGINAL_DST (Linux-specific) to recover the
// original VIP after iptables REDIRECT; on other OSes it can't function.
var errOrigDstUnsupported = errors.New("origDst: SO_ORIGINAL_DST is only supported on Linux")

func origDst(c net.Conn) (*net.TCPAddr, error) {
	return nil, errOrigDstUnsupported
}
