package runtime

import (
	"errors"
	"net/netip"
)

// inet and cidr decoding.
//
// Both map to netip.Prefix — ONE Go type, not two. An inet is an address that
// may carry a prefix length (host bits allowed); a cidr is a network (host
// bits forbidden, enforced by the database). netip.Prefix represents both
// exactly, and callers who want just the address call .Addr(). Two Go types
// would double the predicate machinery to encode a distinction the database
// already polices.

// pgx's wire families, which are NOT the OS's AF_* constants.
const (
	pgInetFamilyV4 = 2
	pgInetFamilyV6 = 3
)

// InetErr decodes an inet or cidr column: family, prefix bits, is_cidr flag,
// address length, then the address bytes.
func InetErr(b []byte) (netip.Prefix, error) {
	if b == nil {
		return netip.Prefix{}, nil
	}
	if len(b) < 4 {
		return netip.Prefix{}, errors.New("storm: inet wire value is too short")
	}
	bits := int(b[1])
	addrLen := int(b[3])
	if len(b) < 4+addrLen {
		return netip.Prefix{}, errors.New("storm: inet wire value is truncated")
	}
	addr, ok := netip.AddrFromSlice(b[4 : 4+addrLen])
	if !ok {
		return netip.Prefix{}, errors.New("storm: inet address is neither 4 nor 16 bytes")
	}
	if bits > addr.BitLen() {
		return netip.Prefix{}, errors.New("storm: inet prefix length exceeds the address width")
	}
	return netip.PrefixFrom(addr, bits), nil
}

// NullInet decodes a nullable inet column.
func NullInet(b []byte) (Null[netip.Prefix], error) {
	if b == nil {
		return Null[netip.Prefix]{}, nil
	}
	p, err := InetErr(b)
	if err != nil {
		return Null[netip.Prefix]{}, err
	}
	return Null[netip.Prefix]{V: p, Valid: true}, nil
}

// Int8Array decodes an int8[] column.
func Int8Array(b []byte) ([]int64, error) {
	return Array(b, Int8)
}
