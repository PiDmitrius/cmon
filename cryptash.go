package main

import (
	"crypto/rand"
	"crypto/sha256"
	"sync"
)

const cryptashHashSize = 32

type cryptashCtx struct {
	mu       sync.Mutex
	password []byte
	ivsz     int
	macsz    int
	k1       [cryptashHashSize]byte
	kn       [cryptashHashSize]byte
	rnd      []byte
	hbuf     []byte
}

func cryptashNew(password []byte, ivsz, macsz int) *cryptashCtx {
	if ivsz > cryptashHashSize/2 {
		ivsz = cryptashHashSize / 2
	}
	if macsz > cryptashHashSize {
		macsz = cryptashHashSize
	}
	return &cryptashCtx{
		password: append([]byte{}, password...),
		ivsz:     ivsz,
		macsz:    macsz,
		rnd:      make([]byte, ivsz*2),
		hbuf:     make([]byte, cryptashHashSize*2),
	}
}

func (c *cryptashCtx) hashInto(dst *[cryptashHashSize]byte, a, b []byte) {
	total := len(a) + len(b)
	var buf []byte
	if total <= len(c.hbuf) {
		buf = c.hbuf[:total]
	} else {
		buf = make([]byte, total)
	}
	copy(buf, a)
	copy(buf[len(a):], b)
	*dst = sha256.Sum256(buf)
}

func (c *cryptashCtx) encrypt(data []byte) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	rand.Read(c.rnd)

	ivsz := c.ivsz
	macsz := c.macsz
	ptLen := ivsz + len(data)
	total := ivsz + macsz + ptLen

	out := make([]byte, total)

	copy(out, c.rnd[:ivsz])
	copy(out[ivsz+macsz:], c.rnd[ivsz:])
	copy(out[ivsz+macsz+ivsz:], data)

	c.hashInto(&c.k1, out[:ivsz], c.password)

	c.hashInto(&c.kn, out[ivsz+macsz:ivsz+macsz+ptLen], c.k1[:])
	copy(out[ivsz:], c.kn[:macsz])

	c.hashInto(&c.kn, out[:ivsz+macsz], c.k1[:])
	ptOff := ivsz + macsz

	for i := 0; ; {
		l := cryptashHashSize
		if ptLen-i < l {
			l = ptLen - i
		}
		for j := 0; j < l; j++ {
			out[ptOff+i+j] ^= c.kn[j]
		}
		i += l
		if i >= ptLen {
			break
		}
		c.hashInto(&c.kn, out[ptOff+i-cryptashHashSize:ptOff+i], c.kn[:])
	}

	return out
}

func (c *cryptashCtx) decrypt(data []byte) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	ivsz := c.ivsz
	macsz := c.macsz

	if len(data) < 2*ivsz+macsz {
		return nil
	}

	prefixLen := ivsz + macsz
	ctLen := len(data) - prefixLen

	c.hashInto(&c.k1, data[:ivsz], c.password)
	c.hashInto(&c.kn, data[:prefixLen], c.k1[:])

	out := make([]byte, ctLen)
	for i := 0; ; {
		l := cryptashHashSize
		if ctLen-i < l {
			l = ctLen - i
		}
		for j := 0; j < l; j++ {
			out[i+j] = data[prefixLen+i+j] ^ c.kn[j]
		}
		i += l
		if i >= ctLen {
			break
		}
		c.hashInto(&c.kn, data[prefixLen+i-cryptashHashSize:prefixLen+i], c.kn[:])
	}

	if macsz > 0 {
		c.hashInto(&c.kn, out[:ctLen], c.k1[:])
		for i := 0; i < macsz; i++ {
			if data[ivsz+i] != c.kn[i] {
				return nil
			}
		}
	}

	if ctLen == ivsz {
		return []byte{}
	}

	return out[ivsz:]
}
