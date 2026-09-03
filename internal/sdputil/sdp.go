// Package sdputil builds and negotiates SDP offers and answers for the audio
// stream (requirements §8, FR-8.2 following RFC 3264).
package sdputil

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pion/sdp/v3"

	"sipclient/internal/media"
)

// Direction is the media direction attribute of a stream.
type Direction string

const (
	SendRecv Direction = "sendrecv"
	SendOnly Direction = "sendonly"
	RecvOnly Direction = "recvonly"
	Inactive Direction = "inactive"
)

// HeldByUs returns the direction to offer when we place the peer on hold. If
// the peer already told us it is only sending, the stream becomes inactive
// rather than sendonly (FR-4.7).
func HeldByUs(peer Direction) Direction {
	if peer == SendOnly || peer == Inactive {
		return Inactive
	}
	return SendOnly
}

// Description is the parsed view of a peer's SDP that we act on.
type Description struct {
	Address     string
	Port        int
	PayloadType []uint8
	RtpMap      map[uint8]string // payload type -> "PCMU/8000"
	Direction   Direction
	PtimeMS     int
	DTMFPayload uint8
	HasDTMF     bool
}

// Offer describes the stream we are advertising.
type Offer struct {
	Address     string
	Port        int
	Codecs      []media.Codec
	DTMFPayload uint8
	PtimeMS     int
	Direction   Direction
	SessionVer  uint64
}

// Build renders an SDP body for an offer or an answer.
func Build(o Offer) ([]byte, error) {
	if o.Address == "" {
		return nil, fmt.Errorf("sdp: local address is empty")
	}
	if len(o.Codecs) == 0 {
		return nil, fmt.Errorf("sdp: no codecs to offer")
	}

	sessID := uint64(time.Now().Unix())
	ver := o.SessionVer
	if ver == 0 {
		ver = sessID
	}

	formats := make([]string, 0, len(o.Codecs)+1)
	attrs := make([]sdp.Attribute, 0, len(o.Codecs)+4)
	for _, c := range o.Codecs {
		formats = append(formats, strconv.Itoa(int(c.PayloadType)))
		attrs = append(attrs, sdp.Attribute{
			Key:   "rtpmap",
			Value: fmt.Sprintf("%d %s/%d", c.PayloadType, c.Name, c.ClockRate),
		})
	}
	if o.DTMFPayload != 0 {
		formats = append(formats, strconv.Itoa(int(o.DTMFPayload)))
		attrs = append(attrs,
			sdp.Attribute{Key: "rtpmap", Value: fmt.Sprintf("%d telephone-event/%d", o.DTMFPayload, media.SampleRate)},
			sdp.Attribute{Key: "fmtp", Value: fmt.Sprintf("%d 0-16", o.DTMFPayload)},
		)
	}
	attrs = append(attrs,
		sdp.Attribute{Key: "ptime", Value: strconv.Itoa(o.PtimeMS)},
		sdp.Attribute{Key: string(o.Direction)},
	)

	desc := &sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      sessID,
			SessionVersion: ver,
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: o.Address,
		},
		SessionName: "sipclient",
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: o.Address},
		},
		TimeDescriptions: []sdp.TimeDescription{{Timing: sdp.Timing{}}},
		MediaDescriptions: []*sdp.MediaDescription{{
			MediaName: sdp.MediaName{
				Media:   "audio",
				Port:    sdp.RangedPort{Value: o.Port},
				Protos:  []string{"RTP", "AVP"},
				Formats: formats,
			},
			Attributes: attrs,
		}},
	}
	return desc.Marshal()
}

// Parse reads a peer's SDP body.
func Parse(body []byte) (Description, error) {
	var d Description
	if len(body) == 0 {
		return d, fmt.Errorf("sdp: empty body")
	}

	var desc sdp.SessionDescription
	if err := desc.Unmarshal(body); err != nil {
		return d, fmt.Errorf("sdp: parse: %w", err)
	}

	if desc.ConnectionInformation != nil && desc.ConnectionInformation.Address != nil {
		d.Address = desc.ConnectionInformation.Address.Address
	}

	var audio *sdp.MediaDescription
	for _, m := range desc.MediaDescriptions {
		if m.MediaName.Media == "audio" {
			audio = m
			break
		}
	}
	if audio == nil {
		return d, fmt.Errorf("sdp: no audio stream offered")
	}

	d.Port = audio.MediaName.Port.Value
	if audio.ConnectionInformation != nil && audio.ConnectionInformation.Address != nil {
		d.Address = audio.ConnectionInformation.Address.Address // media level wins
	}
	if d.Address == "" {
		return d, fmt.Errorf("sdp: no connection address")
	}

	for _, f := range audio.MediaName.Formats {
		pt, err := strconv.Atoi(f)
		if err != nil || pt < 0 || pt > 127 {
			continue
		}
		d.PayloadType = append(d.PayloadType, uint8(pt))
	}

	d.RtpMap = make(map[uint8]string)
	d.Direction = SendRecv // RFC 3264: absent direction means sendrecv
	d.PtimeMS = 20

	for _, a := range audio.Attributes {
		switch a.Key {
		case "rtpmap":
			pt, enc, ok := splitRtpmap(a.Value)
			if !ok {
				continue
			}
			d.RtpMap[pt] = enc
			if strings.HasPrefix(strings.ToLower(enc), "telephone-event") {
				d.DTMFPayload, d.HasDTMF = pt, true
			}
		case "ptime":
			if v, err := strconv.Atoi(strings.TrimSpace(a.Value)); err == nil && v > 0 {
				d.PtimeMS = v
			}
		case "sendrecv", "sendonly", "recvonly", "inactive":
			d.Direction = Direction(a.Key)
		}
	}

	// A zero port is the classic "stream disabled" signal.
	if d.Port == 0 {
		d.Direction = Inactive
	}
	return d, nil
}

func splitRtpmap(value string) (pt uint8, encoding string, ok bool) {
	fields := strings.Fields(value)
	if len(fields) < 2 {
		return 0, "", false
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil || n < 0 || n > 127 {
		return 0, "", false
	}
	return uint8(n), fields[1], true
}

// Negotiate picks the codec to use: the first of our configured codecs that the
// peer also offered, preserving our preference order (FR-8.2). It returns a
// distinguishable error when there is no overlap so the caller can answer 488.
func Negotiate(ours []media.Codec, peer Description) (media.Codec, error) {
	offered := make(map[uint8]bool, len(peer.PayloadType))
	for _, pt := range peer.PayloadType {
		offered[pt] = true
	}
	for _, c := range ours {
		if offered[c.PayloadType] {
			return c, nil
		}
	}
	return media.Codec{}, ErrNoCommonCodec
}

// ErrNoCommonCodec means the offer and our configuration do not intersect.
var ErrNoCommonCodec = fmt.Errorf("no common codec")

// DTMFPayloadFor returns the telephone-event payload type to transmit with:
// the peer's if it named one, otherwise our default.
func DTMFPayloadFor(peer Description) uint8 {
	if peer.HasDTMF {
		return peer.DTMFPayload
	}
	return media.DTMFPayloadType
}
