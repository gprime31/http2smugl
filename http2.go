package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type RequestParams struct {
	Target, Method, ConnectAddr string
	Headers                     Headers
	NoAutoHeaders               bool
	Body                        []byte
	Timeout                     time.Duration
	AddContentLength            bool
}

type RSTError struct {
	Code http2.ErrCode
}

func (r RSTError) Error() string {
	return fmt.Sprintf("received RST frame, code=%v", r.Code)
}

func DoRequest(params *RequestParams) (Headers, []byte, error) {
	parsed, err := url.Parse(params.Target)
	if err != nil {
		return nil, nil, err
	}

	var (
		noTLS  bool
		scheme string
	)

	switch parsed.Scheme {
	case "http+h2c", "h2c":
		noTLS = true
		scheme = "http"
	case "https", "h2":
		scheme = "https"
	default:
		return nil, nil, fmt.Errorf(`scheme is %#v, must be "https" or "http+h2c"`, parsed.Scheme)
	}

	var headers Headers

	if params.NoAutoHeaders {
		headers = params.Headers
	} else {
		headers = Headers{
			{":authority", parsed.Host},
			{":method", params.Method},
			{":path", parsed.Path},
			{":scheme", scheme},
			{"user-agent", "Mozilla/5.0"},
		}

		toSkip := make(map[string]struct{})
		for i := range headers {
			h := &headers[i]
			if v, ok := params.Headers.Get(h.Name); ok {
				h.Value = v
				toSkip[h.Name] = struct{}{}
			}
		}

		for _, h := range params.Headers {
			if _, ok := toSkip[h.Name]; ok {
				delete(toSkip, h.Name)
				continue
			}
			headers = append(headers, h)
		}
	}

	if params.AddContentLength {
		headers = append(headers, Header{"content-length", strconv.Itoa(len(params.Body))})
	}

	targetAddr := params.ConnectAddr
	if targetAddr == "" {
		targetAddr = parsed.Host
	}

	return sendPreparedRequest(targetAddr, parsed.Host, noTLS, prepareRequest(headers, params.Body), params.Timeout)
}

func prepareRequest(headers Headers, body []byte) []byte {
	var hpackBuf []byte
	for i := range headers {
		hpackBuf = hpackAppendHeader(hpackBuf, &headers[i])
	}

	requestBuf := bytes.NewBuffer(nil)
	requestBuf.Write([]byte(http2.ClientPreface))

	framer := http2.NewFramer(requestBuf, nil)

	_ = framer.WriteSettings(http2.Setting{
		ID:  http2.SettingInitialWindowSize,
		Val: (1 << 31) - 1,
	})

	_ = framer.WriteWindowUpdate(0, (1<<31)-(1<<16)-1)

	_ = framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      1,
		BlockFragment: hpackBuf,
		EndStream:     len(body) == 0,
		EndHeaders:    true,
	})

	start := 0
	for start < len(body) {
		end := start + 65536
		if end > len(body) {
			end = len(body)
		}
		_ = framer.WriteData(1, end == len(body), body[start:end])
		start = end
	}

	_ = framer.WriteSettingsAck()

	return requestBuf.Bytes()
}

func sendPreparedRequest(connectAddr, serverName string, noTLS bool, request []byte, timeout time.Duration) (headers Headers, body []byte, err error) {
	address := connectAddr
	if _, _, err := net.SplitHostPort(connectAddr); err != nil {
		address = net.JoinHostPort(address, "443")
	}

	name, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid address %v: %w", address, err)
	}
	ip, err := DefaultDNSCache.Lookup(name)
	if err != nil {
		return nil, nil, fmt.Errorf("lookup for %v failed: %w", name, err)
	}
	address = net.JoinHostPort(ip.String(), port)

	tcpConn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return nil, nil, err
	}

	defer func() { _ = tcpConn.Close() }()
	_ = tcpConn.SetDeadline(time.Now().Add(timeout))

	var c net.Conn

	if noTLS {
		c = tcpConn
	} else {
		c = tls.Client(tcpConn, &tls.Config{
			NextProtos:         []string{"h2"},
			ServerName:         serverName,
			InsecureSkipVerify: true,
		})
	}

	if _, err := c.Write(request); err != nil {
		return nil, nil, err
	}

	headersDecoder := hpack.NewDecoder(^uint32(0), func(f hpack.HeaderField) {
		headers = append(headers, Header{f.Name, f.Value})
	})

	framer := http2.NewFramer(nil, c)

	hasBody := false
	bodyRead := false
	headersDone := false
	for !headersDone || (hasBody && !bodyRead) {
		var f http2.Frame
		f, err = framer.ReadFrame()
		if err != nil {
			return
		}

		if f.Header().StreamID != 1 {
			continue
		}

		switch f := f.(type) {
		case *http2.HeadersFrame:
			if _, err := headersDecoder.Write(f.HeaderBlockFragment()); err != nil {
				return nil, nil, err
			}
			headersDone = f.HeadersEnded()
			hasBody = !f.StreamEnded()

		case *http2.ContinuationFrame:
			if _, err := headersDecoder.Write(f.HeaderBlockFragment()); err != nil {
				return nil, nil, err
			}
			headersDone = f.HeadersEnded()

		case *http2.DataFrame:
			// we should send window update, but who cares
			body = append(body, f.Data()...)
			bodyRead = f.StreamEnded()

		case *http2.RSTStreamFrame:
			err = RSTError{Code: f.ErrCode}
			return
		}

	}

	return
}

func hpackAppendHeader(dst []byte, h *Header) []byte {
	dst = append(dst, 0x10)
	dst = hpackAppendVarInt(dst, 7, uint64(len(h.Name)))
	dst = append(dst, h.Name...)
	dst = hpackAppendVarInt(dst, 7, uint64(len(h.Value)))
	dst = append(dst, h.Value...)
	return dst
}

func hpackAppendVarInt(dst []byte, n byte, val uint64) []byte {
	k := uint64((1 << n) - 1)
	if val < k {
		return append(dst, byte(val))
	}
	dst = append(dst, byte(k))
	val -= k
	for ; val >= 128; val >>= 7 {
		dst = append(dst, byte(0x80|(val&0x7f)))
	}
	return append(dst, byte(val))
}
