//go:build linux

package proxy

import (
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

// soOriginalDst is the Linux IPv4 option number for retrieving the
// original destination address before iptables NAT mangled it. Used after
// REDIRECT / DNAT. The Go stdlib does not yet expose this constant.
const soOriginalDst = 80

// origDst returns the original IPv4 destination of a redirected TCP
// connection, recovered via getsockopt(SO_ORIGINAL_DST). The connection
// must be a *net.TCPConn that arrived via an iptables REDIRECT or DNAT
// rule; otherwise the returned address is the local listener's bind
// address (which is not useful for upstream routing).
func origDst(c net.Conn) (*net.TCPAddr, error) {
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		return nil, fmt.Errorf("origDst: connection is not *net.TCPConn (%T)", c)
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("origDst: SyscallConn: %w", err)
	}

	var (
		sa    syscall.RawSockaddrInet4
		soErr error
	)
	ctrlErr := raw.Control(func(fd uintptr) {
		size := uint32(syscall.SizeofSockaddrInet4)
		_, _, errno := syscall.Syscall6(
			syscall.SYS_GETSOCKOPT,
			fd,
			uintptr(syscall.IPPROTO_IP),
			uintptr(soOriginalDst),
			uintptr(unsafe.Pointer(&sa)),
			uintptr(unsafe.Pointer(&size)),
			0,
		)
		if errno != 0 {
			soErr = errno
		}
	})
	if ctrlErr != nil {
		return nil, fmt.Errorf("origDst: control: %w", ctrlErr)
	}
	if soErr != nil {
		return nil, fmt.Errorf("origDst: getsockopt SO_ORIGINAL_DST: %w", soErr)
	}

	// sa.Port is in network byte order; the high byte is first.
	port := int(sa.Port>>8&0xff | sa.Port<<8&0xff00)
	ip := net.IPv4(sa.Addr[0], sa.Addr[1], sa.Addr[2], sa.Addr[3])
	return &net.TCPAddr{IP: ip, Port: port}, nil
}
