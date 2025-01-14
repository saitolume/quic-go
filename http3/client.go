package http3

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/internal/protocol"
	"github.com/lucas-clemente/quic-go/internal/qtls"
	"github.com/lucas-clemente/quic-go/internal/utils"
	"github.com/marten-seemann/qpack"
)

// MethodGet0RTT allows a GET request to be sent using 0-RTT.
// Note that 0-RTT data doesn't provide replay protection.
const MethodGet0RTT = "GET_0RTT"

const defaultUserAgent = "quic-go HTTP/3"

var defaultQuicConfig = &quic.Config{
	MaxIncomingStreams: -1, // don't allow the server to create bidirectional streams
	KeepAlive:          true,
	Versions:           []protocol.VersionNumber{protocol.VersionTLS},
}

var dialAddr = quic.DialAddrEarly

// client is a HTTP3 client doing requests
type client struct {
	tlsConf  *tls.Config
	config   *quic.Config
	settings Settings

	disableCompression bool

	dialOnce     sync.Once
	dialer       func(network, addr string, tlsCfg *tls.Config, cfg *quic.Config) (quic.EarlySession, error)
	handshakeErr error

	authority string
	sess      quic.EarlySession
	conn      ClientConn

	logger utils.Logger
}

func newClient(
	authority string,
	tlsConf *tls.Config,
	quicConfig *quic.Config,
	settings Settings,
	disableCompression bool,
	dialer func(network, addr string, tlsCfg *tls.Config, cfg *quic.Config) (quic.EarlySession, error),
) (*client, error) {
	if quicConfig == nil {
		quicConfig = defaultQuicConfig.Clone()
	} else if len(quicConfig.Versions) == 0 {
		quicConfig = quicConfig.Clone()
		quicConfig.Versions = []quic.VersionNumber{defaultQuicConfig.Versions[0]}
	}
	if len(quicConfig.Versions) != 1 {
		return nil, errors.New("can only use a single QUIC version for dialing a HTTP/3 connection")
	}

	// Don’t allow incoming bidirectional streams unless WebTransport is enabled.
	if !settings.WebTransportEnabled() {
		quicConfig.MaxIncomingStreams = -1
	}
	quicConfig.EnableDatagrams = settings.DatagramsEnabled()
	logger := utils.DefaultLogger.WithPrefix("h3 client")

	if tlsConf == nil {
		tlsConf = &tls.Config{}
	} else {
		tlsConf = tlsConf.Clone()
	}
	// Replace existing ALPNs by H3
	tlsConf.NextProtos = []string{versionToALPN(quicConfig.Versions[0])}

	return &client{
		authority:          authorityAddr("https", authority),
		tlsConf:            tlsConf,
		config:             quicConfig,
		settings:           settings,
		disableCompression: disableCompression,
		dialer:             dialer,
		logger:             logger,
	}, nil
}

func (c *client) dial() error {
	var err error
	if c.dialer != nil {
		c.sess, err = c.dialer("udp", c.authority, c.tlsConf, c.config)
	} else {
		c.sess, err = dialAddr(c.authority, c.tlsConf, c.config)
	}
	if err != nil {
		return err
	}

	c.conn, err = Open(c.sess, c.settings)
	if err != nil {
		c.logger.Errorf("unable to open HTTP/3 connection: %s", err)
		c.sess.CloseWithError(quic.ApplicationErrorCode(errorInternalError), "")
		return err
	}

	return nil
}

func (c *client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.sess.CloseWithError(quic.ApplicationErrorCode(errorNoError), "")
}

func (c *client) RoundTrip(req *http.Request) (*http.Response, error) {
	if authorityAddr("https", hostnameFromRequest(req)) != c.authority {
		return nil, fmt.Errorf("http3 client BUG: RoundTrip called for the wrong client (expected %s, got %s)", c.authority, req.Host)
	}

	c.dialOnce.Do(func() {
		c.handshakeErr = c.dial()
	})

	if c.handshakeErr != nil {
		return nil, c.handshakeErr
	}

	// Immediately send out this request, if this is a 0-RTT request.
	if req.Method == MethodGet0RTT {
		req.Method = http.MethodGet
	} else {
		// wait for the handshake to complete
		select {
		case <-c.sess.HandshakeComplete().Done():
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}

	str, err := c.conn.OpenRequestStream(req.Context())
	if err != nil {
		return nil, err
	}

	// Request Cancellation:
	// This go routine keeps running even after RoundTrip() returns.
	// It is shut down when the application is done processing the body.
	reqDone := make(chan struct{})
	go func() {
		select {
		case <-req.Context().Done():
			str.CancelWrite(quic.StreamErrorCode(errorRequestCanceled))
			str.CancelRead(quic.StreamErrorCode(errorRequestCanceled))
		case <-reqDone:
		}
	}()

	res, err := c.doRequest(str, req, reqDone)
	if err != nil {
		close(reqDone)
		switch err := err.(type) {
		case *FrameTypeError:
			// HTTP responses MUST start with a HEADERS frame.
			c.sess.CloseWithError(quic.ApplicationErrorCode(errorFrameUnexpected), err.Error())
		case *FrameLengthError:
			str.CancelWrite(quic.StreamErrorCode(errorFrameError))
		default:
			str.CancelWrite(quic.StreamErrorCode(errorGeneralProtocolError))
		}
	}
	return res, err
}

func (c *client) doRequest(
	str RequestStream,
	req *http.Request,
	reqDone chan struct{},
) (*http.Response, error) {
	var requestGzip bool
	if !c.disableCompression && req.Method != http.MethodHead && req.Header.Get("Accept-Encoding") == "" && req.Header.Get("Range") == "" {
		requestGzip = true
	}

	err := c.writeRequest(str, req, requestGzip)
	if err != nil {
		return nil, err
	}

	// Read HEADERS frames until we get a non-interim status code.
	res := &http.Response{
		Proto:      "HTTP/3",
		ProtoMajor: 3,
		Header:     http.Header{},
	}
	for {
		headers, err := str.ReadHeaders()
		if err != nil {
			return nil, err
		}

		var statusCode int
		var status string
		for _, hf := range headers {
			if hf.Name == pseudoHeaderStatus {
				statusCode, err = strconv.Atoi(hf.Value)
				if err != nil {
					// A malformed :status header is an H3_MESSAGE_ERROR.
					// TODO(ydnar): a server MAY send a response indicating the
					// error before closing or resetting the stream.
					// See https://quicwg.org/base-drafts/draft-ietf-quic-http.html#malformed.
					str.CancelWrite(quic.StreamErrorCode(errorMessageError))
					return nil, errors.New("invalid or missing :status header")
				}
				status = hf.Value + " " + http.StatusText(res.StatusCode)
			} else {
				res.Header.Add(hf.Name, hf.Value)
			}
		}

		if statusCode < 100 || statusCode >= 200 {
			res.StatusCode = statusCode
			res.Status = status
			break
		}
	}

	connState := qtls.ToTLSConnectionState(c.sess.ConnectionState().TLS)
	res.TLS = &connState

	onTrailers := func(fields []qpack.HeaderField, err error) {
		if err != nil {
			c.logger.Errorf("error reading trailer: %s", err)
			return
		}
		c.logger.Debugf("read %d trailer fields", len(fields))
		if res.Trailer == nil {
			res.Trailer = http.Header{}
		}
		for _, f := range fields {
			res.Trailer.Add(f.Name, f.Value)
		}
	}

	respBody := newResponseBody(str, onTrailers, reqDone)

	// Rules for when to set Content-Length are defined in https://tools.ietf.org/html/rfc7230#section-3.3.2.
	_, hasTransferEncoding := res.Header["Transfer-Encoding"]
	isInformational := res.StatusCode >= 100 && res.StatusCode < 200
	isNoContent := res.StatusCode == 204
	isSuccessfulConnect := req.Method == http.MethodConnect && res.StatusCode >= 200 && res.StatusCode < 300
	if !hasTransferEncoding && !isInformational && !isNoContent && !isSuccessfulConnect {
		res.ContentLength = -1
		if clens, ok := res.Header["Content-Length"]; ok && len(clens) == 1 {
			if clen64, err := strconv.ParseInt(clens[0], 10, 64); err == nil {
				res.ContentLength = clen64
			}
		}
	}

	if requestGzip && res.Header.Get("Content-Encoding") == "gzip" {
		res.Header.Del("Content-Encoding")
		res.Header.Del("Content-Length")
		res.ContentLength = -1
		res.Body = newGzipReader(respBody)
		res.Uncompressed = true
	} else {
		res.Body = respBody
	}

	return res, nil
}

func (c *client) writeRequest(str RequestStream, req *http.Request, requestGzip bool) error {
	fields, err := RequestHeaders(req)
	if err != nil {
		return err
	}

	if requestGzip {
		fields = appendGzipHeader(fields)
	}

	err = str.WriteHeaders(fields)
	if err != nil {
		return err
	}

	if req.Body == nil && len(req.Trailer) == 0 {
		if req.Method != http.MethodConnect {
			str.Close()
		}
		return nil
	}

	// Send the request body and trailers asynchronously
	go func() {
		_, err := io.Copy(str.DataWriter(), req.Body)
		req.Body.Close()
		if err != nil {
			c.logger.Errorf("Error writing request: %s", err)
			str.CancelWrite(quic.StreamErrorCode(errorRequestCanceled))
			return
		}

		if len(req.Trailer) > 0 {
			err = str.WriteHeaders(Trailers(req.Trailer))
			if err != nil {
				c.logger.Errorf("Error writing trailers: %s", err)
				str.CancelWrite(quic.StreamErrorCode(errorRequestCanceled))
				return
			}
		}

		if req.Method != http.MethodConnect {
			str.Close()
		}
	}()

	return nil
}
