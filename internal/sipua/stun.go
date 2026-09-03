package sipua

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

// discoverPublicAddress performs a minimal RFC 5389 binding request to learn
// our mapped address. Only XOR-MAPPED-ADDRESS/IPv4 is handled, which is all a
// SIP client needs to fill in Contact and the SDP connection line.
func discoverPublicAddress(stunServer string, timeout time.Duration) (string, error) {
	conn, err := net.DialTimeout("udp", stunServer, timeout)
	if err != nil {
		return "", fmt.Errorf("stun: dial %s: %w", stunServer, err)
	}
	defer conn.Close()

	const (
		bindingRequest = 0x0001
		magicCookie    = 0x2112A442
		xorMapped      = 0x0020
		mapped         = 0x0001
	)

	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:], bindingRequest)
	binary.BigEndian.PutUint16(req[2:], 0) // no attributes
	binary.BigEndian.PutUint32(req[4:], magicCookie)
	if _, err := rand.Read(req[8:20]); err != nil {
		return "", fmt.Errorf("stun: transaction id: %w", err)
	}

	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(req); err != nil {
		return "", fmt.Errorf("stun: write: %w", err)
	}

	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		return "", fmt.Errorf("stun: read: %w", err)
	}
	if n < 20 {
		return "", fmt.Errorf("stun: short response (%d bytes)", n)
	}

	body := buf[20:n]
	for len(body) >= 4 {
		attrType := binary.BigEndian.Uint16(body[0:])
		attrLen := int(binary.BigEndian.Uint16(body[2:]))
		if 4+attrLen > len(body) {
			break
		}
		val := body[4 : 4+attrLen]

		if (attrType == xorMapped || attrType == mapped) && len(val) >= 8 && val[1] == 0x01 {
			ip := make(net.IP, 4)
			copy(ip, val[4:8])
			if attrType == xorMapped {
				cookie := make([]byte, 4)
				binary.BigEndian.PutUint32(cookie, magicCookie)
				for i := range ip {
					ip[i] ^= cookie[i]
				}
			}
			return ip.String(), nil
		}

		// Attributes are padded to a 4-byte boundary.
		advance := 4 + attrLen
		if pad := attrLen % 4; pad != 0 {
			advance += 4 - pad
		}
		if advance > len(body) {
			break
		}
		body = body[advance:]
	}
	return "", fmt.Errorf("stun: no mapped address in response")
}
