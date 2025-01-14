package http3

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/lucas-clemente/quic-go"
	mockquic "github.com/lucas-clemente/quic-go/internal/mocks/quic"
	"github.com/lucas-clemente/quic-go/quicvarint"

	"github.com/lucas-clemente/quic-go/internal/protocol"
	"github.com/lucas-clemente/quic-go/internal/utils"
	"github.com/marten-seemann/qpack"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Client", func() {
	var (
		client       *client
		req          *http.Request
		origDialAddr = dialAddr
		handshakeCtx context.Context // an already canceled context
	)

	BeforeEach(func() {
		origDialAddr = dialAddr
		hostname := "quic.clemente.io:1337"
		var err error
		client, err = newClient(hostname, nil, nil, nil, false, nil)
		Expect(err).ToNot(HaveOccurred())
		Expect(client.authority).To(Equal(hostname))
		req, err = http.NewRequest("GET", "https://localhost:1337", nil)
		Expect(err).ToNot(HaveOccurred())

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		handshakeCtx = ctx
	})

	AfterEach(func() {
		dialAddr = origDialAddr
	})

	Context("Settings", func() {
		It("has nil Settings when none is provided", func() {
			client, err := newClient("example.com", nil, nil, nil, false, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(client.settings).To(BeNil())
		})

		It("passes configured Settings through exactly", func() {
			settings := Settings{1: 1, 2: 2}
			client, err := newClient("example.com", nil, nil, settings, false, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(client.settings).To(Equal(settings))
		})

		It("rejects quic.Configs that allow multiple QUIC versions", func() {
			qconf := &quic.Config{
				Versions: []quic.VersionNumber{protocol.VersionDraft29, protocol.Version1},
			}
			_, err := newClient("localhost:1337", nil, qconf, nil, false, nil)
			Expect(err).To(MatchError("can only use a single QUIC version for dialing a HTTP/3 connection"))
		})
	})

	It("uses the default QUIC and TLS config if none is give", func() {
		client, err := newClient("localhost:1337", nil, nil, nil, false, nil)
		Expect(err).ToNot(HaveOccurred())
		var dialAddrCalled bool
		dialAddr = func(_ string, tlsConf *tls.Config, quicConf *quic.Config) (quic.EarlySession, error) {
			Expect(quicConf).To(Equal(defaultQuicConfig))
			Expect(tlsConf.NextProtos).To(Equal([]string{nextProtoH3}))
			Expect(quicConf.Versions).To(Equal([]protocol.VersionNumber{protocol.Version1}))
			dialAddrCalled = true
			return nil, errors.New("test done")
		}
		client.RoundTrip(req)
		Expect(dialAddrCalled).To(BeTrue())
	})

	It("adds the port to the hostname, if none is given", func() {
		client, err := newClient("quic.clemente.io", nil, nil, nil, false, nil)
		Expect(err).ToNot(HaveOccurred())
		var dialAddrCalled bool
		dialAddr = func(hostname string, _ *tls.Config, _ *quic.Config) (quic.EarlySession, error) {
			Expect(hostname).To(Equal("quic.clemente.io:443"))
			dialAddrCalled = true
			return nil, errors.New("test done")
		}
		req, err := http.NewRequest("GET", "https://quic.clemente.io:443", nil)
		Expect(err).ToNot(HaveOccurred())
		client.RoundTrip(req)
		Expect(dialAddrCalled).To(BeTrue())
	})

	It("uses the TLS config and QUIC config", func() {
		tlsConf := &tls.Config{
			ServerName: "foo.bar",
			NextProtos: []string{"proto foo", "proto bar"},
		}
		quicConf := &quic.Config{MaxIdleTimeout: time.Nanosecond}
		client, err := newClient("localhost:1337", tlsConf, quicConf, nil, false, nil)
		Expect(err).ToNot(HaveOccurred())
		var dialAddrCalled bool
		dialAddr = func(
			hostname string,
			tlsConfP *tls.Config,
			quicConfP *quic.Config,
		) (quic.EarlySession, error) {
			Expect(hostname).To(Equal("localhost:1337"))
			Expect(tlsConfP.ServerName).To(Equal(tlsConf.ServerName))
			Expect(tlsConfP.NextProtos).To(Equal([]string{nextProtoH3}))
			Expect(quicConfP.MaxIdleTimeout).To(Equal(quicConf.MaxIdleTimeout))
			dialAddrCalled = true
			return nil, errors.New("test done")
		}
		client.RoundTrip(req)
		Expect(dialAddrCalled).To(BeTrue())
		// make sure the original tls.Config was not modified
		Expect(tlsConf.NextProtos).To(Equal([]string{"proto foo", "proto bar"}))
	})

	It("uses the custom dialer, if provided", func() {
		testErr := errors.New("test done")
		tlsConf := &tls.Config{ServerName: "foo.bar"}
		quicConf := &quic.Config{MaxIdleTimeout: 1337 * time.Second}
		var dialerCalled bool
		dialer := func(network, address string, tlsConfP *tls.Config, quicConfP *quic.Config) (quic.EarlySession, error) {
			Expect(network).To(Equal("udp"))
			Expect(address).To(Equal("localhost:1337"))
			Expect(tlsConfP.ServerName).To(Equal("foo.bar"))
			Expect(quicConfP.MaxIdleTimeout).To(Equal(quicConf.MaxIdleTimeout))
			dialerCalled = true
			return nil, testErr
		}
		client, err := newClient("localhost:1337", tlsConf, quicConf, nil, false, dialer)
		Expect(err).ToNot(HaveOccurred())
		_, err = client.RoundTrip(req)
		Expect(err).To(MatchError(testErr))
		Expect(dialerCalled).To(BeTrue())
	})

	It("enables HTTP/3 Datagrams", func() {
		testErr := errors.New("handshake error")
		settings := Settings{}
		settings.EnableDatagrams()
		client, err := newClient("localhost:1337", nil, nil, settings, false, nil)
		Expect(err).ToNot(HaveOccurred())
		dialAddr = func(hostname string, _ *tls.Config, quicConf *quic.Config) (quic.EarlySession, error) {
			Expect(quicConf.EnableDatagrams).To(BeTrue())
			return nil, testErr
		}
		_, err = client.RoundTrip(req)
		Expect(err).To(MatchError(testErr))
	})

	It("errors when dialing fails", func() {
		testErr := errors.New("handshake error")
		client, err := newClient("localhost:1337", nil, nil, nil, false, nil)
		Expect(err).ToNot(HaveOccurred())
		dialAddr = func(hostname string, _ *tls.Config, _ *quic.Config) (quic.EarlySession, error) {
			return nil, testErr
		}
		_, err = client.RoundTrip(req)
		Expect(err).To(MatchError(testErr))
	})

	It("closes correctly if session was not created", func() {
		client, err := newClient("localhost:1337", nil, nil, nil, false, nil)
		Expect(err).ToNot(HaveOccurred())
		Expect(client.Close()).To(Succeed())
	})

	Context("validating the address", func() {
		It("refuses to do requests for the wrong host", func() {
			req, err := http.NewRequest("https", "https://quic.clemente.io:1336/foobar.html", nil)
			Expect(err).ToNot(HaveOccurred())
			_, err = client.RoundTrip(req)
			Expect(err).To(MatchError("http3 client BUG: RoundTrip called for the wrong client (expected quic.clemente.io:1337, got quic.clemente.io:1336)"))
		})

		It("allows requests using a different scheme", func() {
			testErr := errors.New("handshake error")
			req, err := http.NewRequest("masque", "masque://quic.clemente.io:1337/foobar.html", nil)
			Expect(err).ToNot(HaveOccurred())
			dialAddr = func(hostname string, _ *tls.Config, _ *quic.Config) (quic.EarlySession, error) {
				return nil, testErr
			}
			_, err = client.RoundTrip(req)
			Expect(err).To(MatchError(testErr))
		})
	})

	Context("control stream handling", func() {
		var (
			request              *http.Request
			sess                 *mockquic.MockEarlySession
			settingsFrameWritten chan struct{}
		)
		testDone := make(chan struct{})

		BeforeEach(func() {
			settingsFrameWritten = make(chan struct{})
			reader, writer := io.Pipe()
			controlStr := mockquic.NewMockStream(mockCtrl)
			controlStr.EXPECT().Write(gomock.Any()).DoAndReturn(writer.Write).AnyTimes()
			go func() {
				defer GinkgoRecover()
				r := quicvarint.NewReader(reader)
				streamType, err := quicvarint.Read(r)
				Expect(err).ToNot(HaveOccurred())
				Expect(streamType).To(BeEquivalentTo(StreamTypeControl))
				settings, err := readSettings(&FrameReader{R: reader})
				Expect(err).ToNot(HaveOccurred())
				Expect(settings).ToNot(BeNil())
				close(settingsFrameWritten)
			}() // SETTINGS frame
			sess = mockquic.NewMockEarlySession(mockCtrl)
			sess.EXPECT().Perspective().Return(quic.PerspectiveClient).AnyTimes()
			sess.EXPECT().Context().Return(context.Background()).AnyTimes()
			sess.EXPECT().OpenUniStream().Return(controlStr, nil)
			sess.EXPECT().HandshakeComplete().Return(handshakeCtx)
			sess.EXPECT().ConnectionState().Return(quic.ConnectionState{})
			sess.EXPECT().OpenStreamSync(gomock.Any()).Return(nil, errors.New("done"))
			dialAddr = func(hostname string, _ *tls.Config, _ *quic.Config) (quic.EarlySession, error) { return sess, nil }
			var err error
			request, err = http.NewRequest("GET", "https://quic.clemente.io:1337/file1.dat", nil)
			Expect(err).ToNot(HaveOccurred())
		})

		AfterEach(func() {
			testDone <- struct{}{}
			Eventually(settingsFrameWritten).Should(BeClosed())
		})

		It("parses the SETTINGS frame", func() {
			buf := &bytes.Buffer{}
			quicvarint.Write(buf, uint64(StreamTypeControl))
			Settings{}.writeFrame(buf)
			controlStr := mockquic.NewMockStream(mockCtrl)
			controlStr.EXPECT().Read(gomock.Any()).DoAndReturn(buf.Read).AnyTimes()
			sess.EXPECT().AcceptUniStream(gomock.Any()).DoAndReturn(func(context.Context) (quic.ReceiveStream, error) {
				return controlStr, nil
			})
			sess.EXPECT().AcceptUniStream(gomock.Any()).DoAndReturn(func(context.Context) (quic.ReceiveStream, error) {
				<-testDone
				return nil, errors.New("test done")
			})
			_, err := client.RoundTrip(request)
			Expect(err).To(MatchError("done"))
			time.Sleep(scaleDuration(20 * time.Millisecond)) // don't EXPECT any calls to sess.CloseWithError
		})

		for _, t := range []StreamType{StreamTypeQPACKEncoder, StreamTypeQPACKDecoder} {
			streamType := t
			name := "encoder"
			if streamType == StreamTypeQPACKDecoder {
				name = "decoder"
			}

			It(fmt.Sprintf("ignores the QPACK %s streams", name), func() {
				buf := &bytes.Buffer{}
				quicvarint.Write(buf, uint64(streamType))
				str := mockquic.NewMockStream(mockCtrl)
				str.EXPECT().Read(gomock.Any()).DoAndReturn(buf.Read).AnyTimes()
				sess.EXPECT().AcceptUniStream(gomock.Any()).DoAndReturn(func(context.Context) (quic.ReceiveStream, error) {
					return str, nil
				})
				sess.EXPECT().AcceptUniStream(gomock.Any()).DoAndReturn(func(context.Context) (quic.ReceiveStream, error) {
					<-testDone
					return nil, errors.New("test done")
				})
				_, err := client.RoundTrip(request)
				Expect(err).To(MatchError("done"))
				time.Sleep(scaleDuration(20 * time.Millisecond)) // don't EXPECT any calls to str.CancelRead
			})
		}

		It("resets streams other than the control stream and the QPACK streams", func() {
			buf := &bytes.Buffer{}
			quicvarint.Write(buf, 1337)
			str := mockquic.NewMockStream(mockCtrl)
			str.EXPECT().Read(gomock.Any()).DoAndReturn(buf.Read).AnyTimes()
			done := make(chan struct{})
			str.EXPECT().CancelRead(quic.StreamErrorCode(errorStreamCreationError)).Do(func(code quic.StreamErrorCode) {
				defer GinkgoRecover()
				close(done)
			})
			sess.EXPECT().AcceptUniStream(gomock.Any()).DoAndReturn(func(context.Context) (quic.ReceiveStream, error) {
				return str, nil
			})
			sess.EXPECT().AcceptUniStream(gomock.Any()).DoAndReturn(func(context.Context) (quic.ReceiveStream, error) {
				<-testDone
				return nil, errors.New("test done")
			}).MinTimes(1)
			_, err := client.RoundTrip(request)
			Expect(err).To(MatchError("done"))
			Eventually(done).Should(BeClosed())
		})

		It("errors when the first frame on the control stream is not a SETTINGS frame", func() {
			buf := &bytes.Buffer{}
			quicvarint.Write(buf, uint64(StreamTypeControl))
			quicvarint.Write(buf, uint64(FrameTypeData))
			quicvarint.Write(buf, uint64(0))
			controlStr := mockquic.NewMockStream(mockCtrl)
			controlStr.EXPECT().Read(gomock.Any()).DoAndReturn(buf.Read).AnyTimes()
			sess.EXPECT().AcceptUniStream(gomock.Any()).DoAndReturn(func(context.Context) (quic.ReceiveStream, error) {
				return controlStr, nil
			})
			sess.EXPECT().AcceptUniStream(gomock.Any()).DoAndReturn(func(context.Context) (quic.ReceiveStream, error) {
				<-testDone
				return nil, errors.New("test done")
			})
			done := make(chan struct{})
			sess.EXPECT().CloseWithError(gomock.Any(), gomock.Any()).Do(func(code quic.ApplicationErrorCode, _ string) {
				defer GinkgoRecover()
				Expect(code).To(BeEquivalentTo(errorMissingSettings))
				close(done)
			})
			_, err := client.RoundTrip(request)
			Expect(err).To(MatchError("done"))
			Eventually(done).Should(BeClosed())
		})

		It("errors when parsing the frame on the control stream fails", func() {
			buf := &bytes.Buffer{}
			quicvarint.Write(buf, uint64(StreamTypeControl))
			b := &bytes.Buffer{}
			Settings{}.writeFrame(b)
			buf.Write(b.Bytes()[:b.Len()-1])
			controlStr := mockquic.NewMockStream(mockCtrl)
			controlStr.EXPECT().Read(gomock.Any()).DoAndReturn(buf.Read).AnyTimes()
			sess.EXPECT().AcceptUniStream(gomock.Any()).DoAndReturn(func(context.Context) (quic.ReceiveStream, error) {
				return controlStr, nil
			})
			sess.EXPECT().AcceptUniStream(gomock.Any()).DoAndReturn(func(context.Context) (quic.ReceiveStream, error) {
				<-testDone
				return nil, errors.New("test done")
			})
			done := make(chan struct{})
			sess.EXPECT().CloseWithError(gomock.Any(), gomock.Any()).Do(func(code quic.ApplicationErrorCode, _ string) {
				defer GinkgoRecover()
				Expect(code).To(BeEquivalentTo(errorMissingSettings))
				close(done)
			})
			_, err := client.RoundTrip(request)
			Expect(err).To(MatchError("done"))
			Eventually(done).Should(BeClosed())
		})

		It("errors when parsing the server opens a push stream", func() {
			buf := &bytes.Buffer{}
			quicvarint.Write(buf, uint64(StreamTypePush))
			controlStr := mockquic.NewMockStream(mockCtrl)
			controlStr.EXPECT().Read(gomock.Any()).DoAndReturn(buf.Read).AnyTimes()
			sess.EXPECT().AcceptUniStream(gomock.Any()).DoAndReturn(func(context.Context) (quic.ReceiveStream, error) {
				return controlStr, nil
			})
			sess.EXPECT().AcceptUniStream(gomock.Any()).DoAndReturn(func(context.Context) (quic.ReceiveStream, error) {
				<-testDone
				return nil, errors.New("test done")
			})
			done := make(chan struct{})
			sess.EXPECT().CloseWithError(gomock.Any(), gomock.Any()).Do(func(code quic.ApplicationErrorCode, _ string) {
				defer GinkgoRecover()
				Expect(code).To(BeEquivalentTo(errorIDError))
				close(done)
			})
			_, err := client.RoundTrip(request)
			Expect(err).To(MatchError("done"))
			Eventually(done).Should(BeClosed())
		})

		It("errors when the server advertises datagram support (and we enabled support for it)", func() {
			client.settings = Settings{}
			client.settings.EnableDatagrams()
			buf := &bytes.Buffer{}
			quicvarint.Write(buf, uint64(StreamTypeControl))
			(Settings{SettingDatagram: 1}).writeFrame(buf)
			controlStr := mockquic.NewMockStream(mockCtrl)
			controlStr.EXPECT().Read(gomock.Any()).DoAndReturn(buf.Read).AnyTimes()
			sess.EXPECT().AcceptUniStream(gomock.Any()).DoAndReturn(func(context.Context) (quic.ReceiveStream, error) {
				return controlStr, nil
			})
			sess.EXPECT().AcceptUniStream(gomock.Any()).DoAndReturn(func(context.Context) (quic.ReceiveStream, error) {
				<-testDone
				return nil, errors.New("test done")
			})
			done := make(chan struct{})
			sess.EXPECT().CloseWithError(gomock.Any(), gomock.Any()).Do(func(code quic.ApplicationErrorCode, reason string) {
				defer GinkgoRecover()
				Expect(code).To(BeEquivalentTo(errorSettingsError))
				Expect(reason).To(Equal("missing QUIC Datagram support"))
				close(done)
			})
			_, err := client.RoundTrip(request)
			Expect(err).To(MatchError("done"))
			Eventually(done).Should(BeClosed())
		})
	})

	Context("writing requests", func() {
		var (
			sess   *mockquic.MockEarlySession
			conn   *connection
			str    *mockquic.MockStream
			rstr   RequestStream
			strBuf *bytes.Buffer
		)

		decodeHeader := func(str io.Reader) map[string]string {
			fr := &FrameReader{R: str}
			var err error
			for err == nil && fr.Type != FrameTypeHeaders {
				err = fr.Next()
				ExpectWithOffset(1, err).ToNot(HaveOccurred())
			}
			ExpectWithOffset(1, fr.Type).To(Equal(FrameTypeHeaders))
			data := make([]byte, fr.N)
			_, err = io.ReadFull(fr, data)
			ExpectWithOffset(1, err).ToNot(HaveOccurred())
			decoder := qpack.NewDecoder(nil)
			hfs, err := decoder.DecodeFull(data)
			ExpectWithOffset(1, err).ToNot(HaveOccurred())
			values := make(map[string]string)
			for _, hf := range hfs {
				values[hf.Name] = hf.Value
			}
			return values
		}

		BeforeEach(func() {
			sess = mockquic.NewMockEarlySession(mockCtrl)
			sess.EXPECT().Context().Return(context.Background()).AnyTimes()
			conn = newMockConn(sess, Settings{}, Settings{})
			str = mockquic.NewMockStream(mockCtrl)
			str.EXPECT().StreamID().AnyTimes()
			rstr = newRequestStream(conn, str, 0, 0)
			strBuf = &bytes.Buffer{}
			str.EXPECT().Write(gomock.Any()).DoAndReturn(func(p []byte) (int, error) {
				return strBuf.Write(p)
			}).AnyTimes()
		})

		It("writes a GET request", func() {
			str.EXPECT().Close()
			req, err := http.NewRequest("GET", "https://quic.clemente.io/index.html?foo=bar", nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(client.writeRequest(rstr, req, false)).To(Succeed())
			headerFields := decodeHeader(strBuf)
			Expect(headerFields).To(HaveKeyWithValue(":authority", "quic.clemente.io"))
			Expect(headerFields).To(HaveKeyWithValue(":method", "GET"))
			Expect(headerFields).To(HaveKeyWithValue(":path", "/index.html?foo=bar"))
			Expect(headerFields).To(HaveKeyWithValue(":scheme", "https"))
			Expect(headerFields).ToNot(HaveKey("accept-encoding"))
		})

		It("writes a POST request", func() {
			closed := make(chan struct{})
			str.EXPECT().Close().Do(func() { close(closed) })
			postData := bytes.NewReader([]byte("foobar"))
			req, err := http.NewRequest("POST", "https://quic.clemente.io/upload.html", postData)
			Expect(err).ToNot(HaveOccurred())
			Expect(client.writeRequest(rstr, req, false)).To(Succeed())

			Eventually(closed).Should(BeClosed())
			headerFields := decodeHeader(strBuf)
			Expect(headerFields).To(HaveKeyWithValue(":method", "POST"))
			Expect(headerFields).To(HaveKey("content-length"))
			contentLength, err := strconv.Atoi(headerFields["content-length"])
			Expect(err).ToNot(HaveOccurred())
			Expect(contentLength).To(BeNumerically(">", 0))

			fr := &FrameReader{R: strBuf}
			err = fr.Next()
			Expect(err).ToNot(HaveOccurred())
			Expect(fr.Type).To(Equal(FrameTypeData))
			Expect(fr.N).To(BeEquivalentTo(6))
		})

		It("writes a POST request, if the Body returns an EOF immediately", func() {
			closed := make(chan struct{})
			str.EXPECT().Close().Do(func() { close(closed) })
			body := bytes.NewBufferString("foobar")
			req, err := http.NewRequest("POST", "https://quic.clemente.io/upload.html", body)
			Expect(err).ToNot(HaveOccurred())
			Expect(client.writeRequest(rstr, req, false)).To(Succeed())

			Eventually(closed).Should(BeClosed())
			headerFields := decodeHeader(strBuf)
			Expect(headerFields).To(HaveKeyWithValue(":method", "POST"))

			fr := &FrameReader{R: strBuf}
			err = fr.Next()
			Expect(err).ToNot(HaveOccurred())
			Expect(fr.Type).To(Equal(FrameTypeData))
			Expect(fr.N).To(BeEquivalentTo(6))
		})

		It("sends cookies", func() {
			str.EXPECT().Close()
			req, err := http.NewRequest("GET", "https://quic.clemente.io/", nil)
			Expect(err).ToNot(HaveOccurred())
			cookie1 := &http.Cookie{
				Name:  "Cookie #1",
				Value: "Value #1",
			}
			cookie2 := &http.Cookie{
				Name:  "Cookie #2",
				Value: "Value #2",
			}
			req.AddCookie(cookie1)
			req.AddCookie(cookie2)
			Expect(client.writeRequest(rstr, req, false)).To(Succeed())
			headerFields := decodeHeader(strBuf)
			Expect(headerFields).To(HaveKeyWithValue("cookie", `Cookie #1="Value #1"; Cookie #2="Value #2"`))
		})

		It("adds the header for gzip support", func() {
			str.EXPECT().Close()
			req, err := http.NewRequest("GET", "https://quic.clemente.io/", nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(client.writeRequest(rstr, req, true)).To(Succeed())
			headerFields := decodeHeader(strBuf)
			Expect(headerFields).To(HaveKeyWithValue("accept-encoding", "gzip"))
		})
	})

	Context("doing requests", func() {
		var (
			request              *http.Request
			str                  *mockquic.MockStream
			sess                 *mockquic.MockEarlySession
			settingsFrameWritten chan struct{}
		)
		testDone := make(chan struct{})

		getHeadersFrame := func(headers map[string]string) []byte {
			buf := &bytes.Buffer{}
			headerBuf := &bytes.Buffer{}
			enc := qpack.NewEncoder(headerBuf)
			for name, value := range headers {
				ExpectWithOffset(1, enc.WriteField(qpack.HeaderField{Name: name, Value: value})).To(Succeed())
			}
			ExpectWithOffset(1, enc.Close()).To(Succeed())
			quicvarint.Write(buf, uint64(FrameTypeHeaders))
			quicvarint.Write(buf, uint64(headerBuf.Len()))
			buf.Write(headerBuf.Bytes())
			return buf.Bytes()
		}

		decodeHeader := func(str io.Reader) map[string]string {
			fields := make(map[string]string)
			decoder := qpack.NewDecoder(nil)

			fr := &FrameReader{R: str}
			var err error
			for err == nil && fr.Type != FrameTypeHeaders {
				err = fr.Next()
				ExpectWithOffset(1, err).ToNot(HaveOccurred())
			}
			ExpectWithOffset(1, fr.Type).To(Equal(FrameTypeHeaders))
			data := make([]byte, fr.N)
			_, err = io.ReadFull(fr, data)
			ExpectWithOffset(1, err).ToNot(HaveOccurred())
			hfs, err := decoder.DecodeFull(data)
			ExpectWithOffset(1, err).ToNot(HaveOccurred())
			for _, p := range hfs {
				fields[p.Name] = p.Value
			}
			return fields
		}

		getResponse := func(status int, header, trailer http.Header, body []byte) []byte {
			buf := &bytes.Buffer{}
			rsess := mockquic.NewMockEarlySession(mockCtrl)
			rsess.EXPECT().Context().Return(context.Background()).AnyTimes()
			rconn := newMockConn(rsess, Settings{}, Settings{})
			rstr := mockquic.NewMockStream(mockCtrl)
			rstr.EXPECT().Write(gomock.Any()).DoAndReturn(buf.Write).AnyTimes()
			mstr := newRequestStream(rconn, rstr, 0, 0)
			rw := newResponseWriter(mstr, utils.DefaultLogger)
			for k, vv := range header {
				rw.header[k] = vv[:]
			}
			for k := range trailer {
				rw.header["Trailer"] = append(rw.header["Trailer"], k)
			}
			rw.WriteHeader(status)
			if body != nil {
				rw.Write(body)
			}
			for k, vv := range trailer {
				rw.header[k] = vv[:]
			}
			rw.writeTrailer()
			// TODO: handle trailers
			rw.Flush()
			return buf.Bytes()
		}

		getSimpleResponse := func(status int) []byte {
			return getResponse(status, nil, nil, nil)
		}

		BeforeEach(func() {
			settingsFrameWritten = make(chan struct{})
			reader, writer := io.Pipe()
			controlStr := mockquic.NewMockStream(mockCtrl)
			controlStr.EXPECT().Write(gomock.Any()).DoAndReturn(writer.Write).AnyTimes()
			go func() {
				defer GinkgoRecover()
				r := quicvarint.NewReader(reader)
				streamType, err := quicvarint.Read(r)
				Expect(err).ToNot(HaveOccurred())
				Expect(streamType).To(BeEquivalentTo(StreamTypeControl))
				settings, err := readSettings(&FrameReader{R: reader})
				Expect(err).ToNot(HaveOccurred())
				Expect(settings).ToNot(BeNil())
				close(settingsFrameWritten)
			}() // SETTINGS frame
			str = mockquic.NewMockStream(mockCtrl)
			str.EXPECT().StreamID().AnyTimes()
			str.EXPECT().Context().Return(context.Background()).AnyTimes()
			sess = mockquic.NewMockEarlySession(mockCtrl)
			sess.EXPECT().Perspective().Return(quic.PerspectiveClient).AnyTimes()
			sess.EXPECT().Context().Return(context.Background()).AnyTimes()
			sess.EXPECT().ConnectionState().Return(quic.ConnectionState{})
			sess.EXPECT().OpenUniStream().Return(controlStr, nil)
			sess.EXPECT().AcceptUniStream(gomock.Any()).DoAndReturn(func(context.Context) (quic.ReceiveStream, error) {
				<-testDone
				return nil, errors.New("test done")
			}).MinTimes(1)
			sess.EXPECT().AcceptStream(gomock.Any()).Return(nil, errors.New("done")).MaxTimes(1)
			dialAddr = func(hostname string, _ *tls.Config, _ *quic.Config) (quic.EarlySession, error) {
				return sess, nil
			}
			var err error
			request, err = http.NewRequest("GET", "https://quic.clemente.io:1337/file1.dat", nil)
			Expect(err).ToNot(HaveOccurred())
		})

		AfterEach(func() {
			testDone <- struct{}{}
			Eventually(settingsFrameWritten).Should(BeClosed())
		})

		It("errors if it can't open a stream", func() {
			testErr := errors.New("stream open error")
			sess.EXPECT().OpenStreamSync(context.Background()).Return(nil, testErr)
			sess.EXPECT().CloseWithError(gomock.Any(), gomock.Any()).MaxTimes(1)
			sess.EXPECT().HandshakeComplete().Return(handshakeCtx)
			_, err := client.RoundTrip(request)
			Expect(err).To(MatchError(testErr))
		})

		It("performs a 0-RTT request", func() {
			testErr := errors.New("stream open error")
			request.Method = MethodGet0RTT
			// don't EXPECT any calls to HandshakeComplete()
			sess.EXPECT().OpenStreamSync(context.Background()).Return(str, nil)
			buf := &bytes.Buffer{}
			str.EXPECT().Write(gomock.Any()).DoAndReturn(buf.Write).AnyTimes()
			str.EXPECT().Close()
			str.EXPECT().CancelWrite(gomock.Any())
			str.EXPECT().StreamID().AnyTimes()
			str.EXPECT().Read(gomock.Any()).DoAndReturn(func([]byte) (int, error) {
				return 0, testErr
			})
			_, err := client.RoundTrip(request)
			Expect(err).To(MatchError(testErr))
			Expect(decodeHeader(buf)).To(HaveKeyWithValue(":method", "GET"))
		})

		It("returns a response", func() {
			resBuf := bytes.NewBuffer(getSimpleResponse(418))
			gomock.InOrder(
				sess.EXPECT().HandshakeComplete().Return(handshakeCtx),
				sess.EXPECT().OpenStreamSync(context.Background()).Return(str, nil),
				sess.EXPECT().ConnectionState().Return(quic.ConnectionState{}),
			)
			str.EXPECT().Write(gomock.Any()).AnyTimes().DoAndReturn(func(p []byte) (int, error) { return len(p), nil })
			str.EXPECT().Close()
			str.EXPECT().StreamID().AnyTimes()
			str.EXPECT().Read(gomock.Any()).DoAndReturn(resBuf.Read).AnyTimes()
			res, err := client.RoundTrip(request)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Proto).To(Equal("HTTP/3"))
			Expect(res.ProtoMajor).To(Equal(3))
			Expect(res.StatusCode).To(Equal(418))
		})

		It("handles interim responses", func() {
			resBuf := &bytes.Buffer{}
			resBuf.Write(getSimpleResponse(http.StatusProcessing))
			resBuf.Write(getResponse(http.StatusEarlyHints, http.Header{"Foo": {"1"}}, nil, nil))
			resBuf.Write(getResponse(http.StatusOK, http.Header{"Bar": {"1"}}, nil, nil))
			gomock.InOrder(
				sess.EXPECT().HandshakeComplete().Return(handshakeCtx),
				sess.EXPECT().OpenStreamSync(context.Background()).Return(str, nil),
				sess.EXPECT().ConnectionState().Return(quic.ConnectionState{}),
			)
			str.EXPECT().Write(gomock.Any()).AnyTimes().DoAndReturn(func(p []byte) (int, error) { return len(p), nil })
			str.EXPECT().Close()
			str.EXPECT().StreamID().AnyTimes()
			str.EXPECT().Read(gomock.Any()).DoAndReturn(resBuf.Read).AnyTimes()
			res, err := client.RoundTrip(request)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.StatusCode).To(Equal(http.StatusOK))
			Expect(res.Header.Get("Foo")).To(Equal("1")) // TODO: should interim response headers accumulate into final response?
			Expect(res.Header.Get("Bar")).To(Equal("1"))
		})

		It("returns responses with an invalid :status header", func() {
			resBuf := bytes.NewBuffer(getSimpleResponse(42))
			gomock.InOrder(
				sess.EXPECT().HandshakeComplete().Return(handshakeCtx),
				sess.EXPECT().OpenStreamSync(context.Background()).Return(str, nil),
				sess.EXPECT().ConnectionState().Return(quic.ConnectionState{}),
			)
			str.EXPECT().Write(gomock.Any()).AnyTimes().DoAndReturn(func(p []byte) (int, error) { return len(p), nil })
			str.EXPECT().Close()
			str.EXPECT().StreamID().AnyTimes()
			str.EXPECT().Read(gomock.Any()).DoAndReturn(resBuf.Read).AnyTimes()
			res, err := client.RoundTrip(request)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.StatusCode).To(Equal(42))
		})

		It("errors on malformed :status headers", func() {
			resBuf := &bytes.Buffer{}
			writeHeadersFrame(resBuf, []qpack.HeaderField{{Name: ":status", Value: "101"}}, http.DefaultMaxHeaderBytes)
			writeHeadersFrame(resBuf, []qpack.HeaderField{{Name: ":status", Value: ""}}, http.DefaultMaxHeaderBytes)
			quicvarint.Write(resBuf, uint64(FrameTypeData))
			quicvarint.Write(resBuf, 0)
			gomock.InOrder(
				sess.EXPECT().HandshakeComplete().Return(handshakeCtx),
				sess.EXPECT().OpenStreamSync(context.Background()).Return(str, nil),
			)
			str.EXPECT().Write(gomock.Any()).AnyTimes().DoAndReturn(func(p []byte) (int, error) { return len(p), nil })
			str.EXPECT().Close()
			str.EXPECT().StreamID().AnyTimes()
			str.EXPECT().Read(gomock.Any()).DoAndReturn(resBuf.Read).AnyTimes()
			str.EXPECT().CancelWrite(quic.StreamErrorCode(errorMessageError))
			str.EXPECT().CancelWrite(quic.StreamErrorCode(errorGeneralProtocolError))
			res, err := client.RoundTrip(request)
			Expect(err).To(HaveOccurred())
			Expect(res).To(BeNil())
		})

		It("returns a response with a body", func() {
			body := []byte("foobar")
			resBuf := bytes.NewBuffer(getResponse(200, nil, nil, body))
			gomock.InOrder(
				sess.EXPECT().HandshakeComplete().Return(handshakeCtx),
				sess.EXPECT().OpenStreamSync(context.Background()).Return(str, nil),
				sess.EXPECT().ConnectionState().Return(quic.ConnectionState{}),
			)
			str.EXPECT().Write(gomock.Any()).AnyTimes().DoAndReturn(func(p []byte) (int, error) { return len(p), nil })
			str.EXPECT().Close()
			str.EXPECT().StreamID().AnyTimes()
			str.EXPECT().Read(gomock.Any()).DoAndReturn(resBuf.Read).AnyTimes()
			res, err := client.RoundTrip(request)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.StatusCode).To(Equal(200))
			resBody, err := io.ReadAll(res.Body)
			Expect(err).ToNot(HaveOccurred())
			Expect(resBody).To(Equal(body))
		})

		It("returns a response with trailers", func() {
			body := []byte("foobar")
			trailer := http.Header{}
			trailer.Add("foo", "1")
			trailer.Add("bar", "1")
			resBuf := bytes.NewBuffer(getResponse(200, nil, trailer, body))
			gomock.InOrder(
				sess.EXPECT().HandshakeComplete().Return(handshakeCtx),
				sess.EXPECT().OpenStreamSync(context.Background()).Return(str, nil),
				sess.EXPECT().ConnectionState().Return(quic.ConnectionState{}),
			)
			str.EXPECT().Write(gomock.Any()).AnyTimes().DoAndReturn(func(p []byte) (int, error) { return len(p), nil })
			str.EXPECT().Close()
			str.EXPECT().StreamID().AnyTimes()
			str.EXPECT().Read(gomock.Any()).DoAndReturn(resBuf.Read).AnyTimes()
			res, err := client.RoundTrip(request)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.StatusCode).To(Equal(200))
			resBody, err := io.ReadAll(res.Body)
			Expect(err).ToNot(HaveOccurred())
			Expect(resBody).To(Equal(body))
			Expect(res.Trailer).To(Equal(trailer))
		})

		Context("requests containing a Body", func() {
			var strBuf *bytes.Buffer

			BeforeEach(func() {
				strBuf = &bytes.Buffer{}
				gomock.InOrder(
					sess.EXPECT().HandshakeComplete().Return(handshakeCtx),
					sess.EXPECT().OpenStreamSync(context.Background()).Return(str, nil),
				)
				body := &mockBody{}
				body.SetData([]byte("request body"))
				var err error
				request, err = http.NewRequest("POST", "https://quic.clemente.io:1337/upload", body)
				Expect(err).ToNot(HaveOccurred())
				str.EXPECT().Write(gomock.Any()).DoAndReturn(strBuf.Write).AnyTimes()
			})

			It("sends a request", func() {
				done := make(chan struct{})
				gomock.InOrder(
					str.EXPECT().Close().Do(func() { close(done) }),
					str.EXPECT().CancelWrite(gomock.Any()).MaxTimes(1), // when reading the response errors
				)
				// the response body is sent asynchronously, while already reading the response
				str.EXPECT().Read(gomock.Any()).DoAndReturn(func([]byte) (int, error) {
					<-done
					return 0, errors.New("test done")
				})
				_, err := client.RoundTrip(request)
				Expect(err).To(MatchError("test done"))
				hfs := decodeHeader(strBuf)
				Expect(hfs).To(HaveKeyWithValue(":method", "POST"))
				Expect(hfs).To(HaveKeyWithValue(":path", "/upload"))
			})

			It("returns the error that occurred when reading the body", func() {
				request.Body.(*mockBody).readErr = errors.New("testErr")
				done := make(chan struct{})
				// str.EXPECT().Close()
				gomock.InOrder(
					str.EXPECT().CancelWrite(quic.StreamErrorCode(errorRequestCanceled)).Do(func(quic.StreamErrorCode) {
						close(done)
					}),
					str.EXPECT().CancelWrite(gomock.Any()),
				)

				// the response body is sent asynchronously, while already reading the response
				str.EXPECT().Read(gomock.Any()).DoAndReturn(func([]byte) (int, error) {
					<-done
					return 0, errors.New("test done")
				})
				_, err := client.RoundTrip(request)
				Expect(err).To(MatchError("test done"))
			})

			It("sets the Content-Length", func() {
				done := make(chan struct{})
				buf := &bytes.Buffer{}
				buf.Write(getHeadersFrame(map[string]string{
					":status":        "200",
					"Content-Length": "1337",
				}))
				quicvarint.Write(buf, uint64(FrameTypeData))
				quicvarint.Write(buf, 6)
				buf.Write([]byte("foobar"))
				str.EXPECT().StreamID().AnyTimes()
				str.EXPECT().Close().Do(func() { close(done) })
				sess.EXPECT().ConnectionState().Return(quic.ConnectionState{})
				str.EXPECT().CancelWrite(gomock.Any()).MaxTimes(1) // when reading the response errors
				// the response body is sent asynchronously, while already reading the response
				str.EXPECT().Read(gomock.Any()).DoAndReturn(buf.Read).AnyTimes()
				req, err := client.RoundTrip(request)
				Expect(err).ToNot(HaveOccurred())
				Expect(req.ContentLength).To(BeEquivalentTo(1337))
				Eventually(done).Should(BeClosed())
			})

			It("closes the connection when the first frame is not a HEADERS frame", func() {
				buf := &bytes.Buffer{}
				quicvarint.Write(buf, uint64(FrameTypeData))
				quicvarint.Write(buf, 0x42)
				sess.EXPECT().CloseWithError(quic.ApplicationErrorCode(errorFrameUnexpected), gomock.Any())
				closed := make(chan struct{})
				str.EXPECT().StreamID().AnyTimes()
				str.EXPECT().Close().Do(func() { close(closed) })
				str.EXPECT().Read(gomock.Any()).DoAndReturn(buf.Read).AnyTimes()
				_, err := client.RoundTrip(request)
				Expect(err).To(MatchError(&FrameTypeError{Want: FrameTypeHeaders, Type: FrameTypeData}))
				Eventually(closed).Should(BeClosed())
			})

			It("cancels the stream when the HEADERS frame is too large", func() {
				buf := &bytes.Buffer{}
				max := defaultMaxFieldSectionSize
				len := max + 1
				quicvarint.Write(buf, uint64(FrameTypeHeaders))
				quicvarint.Write(buf, uint64(len))
				str.EXPECT().CancelWrite(quic.StreamErrorCode(errorFrameError))
				closed := make(chan struct{})
				str.EXPECT().StreamID().AnyTimes()
				str.EXPECT().Close().Do(func() { close(closed) })
				str.EXPECT().Read(gomock.Any()).DoAndReturn(buf.Read).AnyTimes()
				_, err := client.RoundTrip(request)
				Expect(err).To(MatchError(&FrameLengthError{
					Type: FrameTypeHeaders,
					Len:  uint64(len),
					Max:  uint64(max),
				}))
				Eventually(closed).Should(BeClosed())
			})
		})

		Context("request cancellations", func() {
			It("cancels a request while waiting for the handshake to complete", func() {
				ctx, cancel := context.WithCancel(context.Background())
				req := request.WithContext(ctx)
				sess.EXPECT().HandshakeComplete().Return(context.Background())

				errChan := make(chan error)
				go func() {
					_, err := client.RoundTrip(req)
					errChan <- err
				}()
				Consistently(errChan).ShouldNot(Receive())
				cancel()
				Eventually(errChan).Should(Receive(MatchError("context canceled")))
			})

			It("cancels a request while the request is still in flight", func() {
				ctx, cancel := context.WithCancel(context.Background())
				req := request.WithContext(ctx)
				sess.EXPECT().HandshakeComplete().Return(handshakeCtx)
				sess.EXPECT().OpenStreamSync(ctx).Return(str, nil)
				buf := &bytes.Buffer{}
				str.EXPECT().Close().MaxTimes(1)
				str.EXPECT().StreamID().AnyTimes()

				str.EXPECT().Write(gomock.Any()).DoAndReturn(buf.Write).MinTimes(1)

				done := make(chan struct{})
				canceled := make(chan struct{})
				gomock.InOrder(
					str.EXPECT().CancelWrite(quic.StreamErrorCode(errorRequestCanceled)).Do(func(quic.StreamErrorCode) { close(canceled) }),
					str.EXPECT().CancelRead(quic.StreamErrorCode(errorRequestCanceled)).Do(func(quic.StreamErrorCode) { close(done) }),
				)
				str.EXPECT().CancelWrite(gomock.Any()).MaxTimes(1)
				str.EXPECT().Read(gomock.Any()).DoAndReturn(func([]byte) (int, error) {
					cancel()
					<-canceled
					return 0, errors.New("test done")
				})
				_, err := client.RoundTrip(req)
				Expect(err).To(MatchError("test done"))
				Eventually(done).Should(BeClosed())
			})

			It("cancels a request after the response arrived", func() {
				resBuf := bytes.NewBuffer(getSimpleResponse(404))

				ctx, cancel := context.WithCancel(context.Background())
				req := request.WithContext(ctx)
				sess.EXPECT().HandshakeComplete().Return(handshakeCtx)
				sess.EXPECT().OpenStreamSync(ctx).Return(str, nil)
				sess.EXPECT().ConnectionState().Return(quic.ConnectionState{})
				buf := &bytes.Buffer{}
				str.EXPECT().Close().MaxTimes(1)
				str.EXPECT().StreamID().AnyTimes()

				done := make(chan struct{})
				str.EXPECT().Write(gomock.Any()).DoAndReturn(buf.Write).MinTimes(1)
				str.EXPECT().Read(gomock.Any()).DoAndReturn(resBuf.Read).AnyTimes()
				str.EXPECT().CancelWrite(quic.StreamErrorCode(errorRequestCanceled))
				str.EXPECT().CancelRead(quic.StreamErrorCode(errorRequestCanceled)).Do(func(quic.StreamErrorCode) { close(done) })
				_, err := client.RoundTrip(req)
				Expect(err).ToNot(HaveOccurred())
				cancel()
				Eventually(done).Should(BeClosed())
			})
		})

		Context("gzip compression", func() {
			BeforeEach(func() {
				sess.EXPECT().HandshakeComplete().Return(handshakeCtx)
			})

			It("adds the gzip header to requests", func() {
				sess.EXPECT().OpenStreamSync(context.Background()).Return(str, nil)
				buf := &bytes.Buffer{}
				str.EXPECT().Write(gomock.Any()).DoAndReturn(buf.Write).MinTimes(1)
				str.EXPECT().StreamID().AnyTimes()
				gomock.InOrder(
					str.EXPECT().Close(),
					str.EXPECT().CancelWrite(gomock.Any()).MaxTimes(1), // when the Read errors
				)
				str.EXPECT().Read(gomock.Any()).Return(0, errors.New("test done"))
				_, err := client.RoundTrip(request)
				Expect(err).To(MatchError("test done"))
				hfs := decodeHeader(buf)
				Expect(hfs).To(HaveKeyWithValue("accept-encoding", "gzip"))
			})

			It("doesn't add gzip if the header disable it", func() {
				client, err := newClient("quic.clemente.io:1337", nil, nil, nil, true, nil)
				Expect(err).ToNot(HaveOccurred())
				sess.EXPECT().OpenStreamSync(context.Background()).Return(str, nil)
				buf := &bytes.Buffer{}
				str.EXPECT().Write(gomock.Any()).DoAndReturn(buf.Write).AnyTimes()
				str.EXPECT().StreamID().AnyTimes()
				gomock.InOrder(
					str.EXPECT().Close(),
					str.EXPECT().CancelWrite(gomock.Any()).MaxTimes(1), // when the Read errors
				)
				str.EXPECT().Read(gomock.Any()).Return(0, errors.New("test done"))
				_, err = client.RoundTrip(request)
				Expect(err).To(MatchError("test done"))
				hfs := decodeHeader(buf)
				Expect(hfs).ToNot(HaveKey("accept-encoding"))
			})

			It("decompresses the response", func() {
				sess.EXPECT().OpenStreamSync(context.Background()).Return(str, nil)
				sess.EXPECT().ConnectionState().Return(quic.ConnectionState{})
				buf := &bytes.Buffer{}
				rsess := mockquic.NewMockEarlySession(mockCtrl)
				rsess.EXPECT().Context().Return(context.Background()).AnyTimes()
				rconn := newMockConn(rsess, Settings{}, Settings{})
				rstr := mockquic.NewMockStream(mockCtrl)
				rstr.EXPECT().Write(gomock.Any()).DoAndReturn(buf.Write).MinTimes(1)
				mstr := newRequestStream(rconn, rstr, 0, 0)
				rw := newResponseWriter(mstr, utils.DefaultLogger)
				rw.Header().Set("Content-Encoding", "gzip")
				gz := gzip.NewWriter(rw)
				gz.Write([]byte("gzipped response"))
				gz.Close()
				rw.Flush()
				str.EXPECT().Write(gomock.Any()).AnyTimes().DoAndReturn(func(p []byte) (int, error) { return len(p), nil })
				str.EXPECT().Read(gomock.Any()).DoAndReturn(buf.Read).AnyTimes()
				str.EXPECT().Close()
				str.EXPECT().StreamID().AnyTimes()

				res, err := client.RoundTrip(request)
				Expect(err).ToNot(HaveOccurred())
				data, err := ioutil.ReadAll(res.Body)
				Expect(err).ToNot(HaveOccurred())
				Expect(res.ContentLength).To(BeEquivalentTo(-1))
				Expect(string(data)).To(Equal("gzipped response"))
				Expect(res.Header.Get("Content-Encoding")).To(BeEmpty())
				Expect(res.Uncompressed).To(BeTrue())
			})

			It("only decompresses the response if the response contains the right content-encoding header", func() {
				sess.EXPECT().OpenStreamSync(context.Background()).Return(str, nil)
				sess.EXPECT().ConnectionState().Return(quic.ConnectionState{})
				buf := &bytes.Buffer{}
				rsess := mockquic.NewMockEarlySession(mockCtrl)
				rsess.EXPECT().Context().Return(context.Background()).AnyTimes()
				rconn := newMockConn(rsess, Settings{}, Settings{})
				rstr := mockquic.NewMockStream(mockCtrl)
				rstr.EXPECT().Write(gomock.Any()).DoAndReturn(buf.Write).AnyTimes()
				mstr := newRequestStream(rconn, rstr, 0, 0)
				rw := newResponseWriter(mstr, utils.DefaultLogger)
				rw.Write([]byte("not gzipped"))
				rw.Flush()
				str.EXPECT().Write(gomock.Any()).AnyTimes().DoAndReturn(func(p []byte) (int, error) { return len(p), nil })
				str.EXPECT().Read(gomock.Any()).DoAndReturn(buf.Read).AnyTimes()
				str.EXPECT().Close()
				str.EXPECT().StreamID().AnyTimes()

				res, err := client.RoundTrip(request)
				Expect(err).ToNot(HaveOccurred())
				data, err := ioutil.ReadAll(res.Body)
				Expect(err).ToNot(HaveOccurred())
				Expect(string(data)).To(Equal("not gzipped"))
				Expect(res.Header.Get("Content-Encoding")).To(BeEmpty())
			})
		})
	})
})
